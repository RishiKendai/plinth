package app_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"plinth/server/internal/platform/config"
	"plinth/server/internal/transactions"
)

// pullAll follows the cursor until the server says it is caught up, like the app does.
func pullAll(t *testing.T, e *env, tok, kind, cursor string, limit int) (ids []string, versions map[string]int64, next string, pages int) {
	t.Helper()
	versions = map[string]int64{}
	for {
		path := "/v1/sync/" + kind + "?limit=" + itoa(limit)
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := e.do(t, "GET", path, tok, nil)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", path, r.Status, r.Raw)
		}
		pages++
		for _, x := range r.Body["data"].([]any) {
			m := x.(map[string]any)
			ids = append(ids, str(m, "id"))
			versions[str(m, "id")] = i64(m, "version")
		}
		cursor = r.Body["next_cursor"].(string)
		if r.Body["has_more"] != true {
			return ids, versions, cursor, pages
		}
		if pages > 100 {
			t.Fatal("pull never finished")
		}
	}
}

func itoa(n int) string { return string(rune('0'+n/10)) + string(rune('0'+n%10)) }

func settleNow(t *testing.T) {
	old := transactions.SyncSettle
	transactions.SyncSettle = 0
	t.Cleanup(func() { transactions.SyncSettle = old })
	time.Sleep(5 * time.Millisecond)
}

func TestSyncPullIsIncrementalAndComplete(t *testing.T) {
	settleNow(t)
	e := setup(t)
	tok, _, _ := e.signUp(t, "asha@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	var created []string
	for i := 0; i < 25; i++ {
		created = append(created, str(e.spend(t, tok, a, int64(100+i), nil).data(), "id"))
	}
	got, _, cur, pages := pullAll(t, e, tok, "transactions", "", 10)
	if len(got) != 25 || pages != 3 {
		t.Fatalf("a full pull in pages of 10: %d rows in %d pages", len(got), pages)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("row %s delivered twice", id)
		}
		seen[id] = true
	}
	for _, id := range created {
		if !seen[id] {
			t.Fatalf("row %s missing", id)
		}
	}
	// Caught up: nothing more.
	if again, _, c2, _ := pullAll(t, e, tok, "transactions", cur, 10); len(again) != 0 || c2 != cur {
		t.Fatalf("nothing changed, nothing returned: %v", again)
	}
	// An edit (a void) and a new row are the only things the next pull returns.
	time.Sleep(5 * time.Millisecond)
	if v := e.call(t, "POST", "/v1/transactions/"+created[3]+"/void", tok, newKey(), nil); v.Status != 200 {
		t.Fatalf("void %d", v.Status)
	}
	fresh := str(e.spend(t, tok, a, 999, nil).data(), "id")
	time.Sleep(5 * time.Millisecond)
	delta, vers, cur2, _ := pullAll(t, e, tok, "transactions", cur, 10)
	if len(delta) != 2 || !contains(delta, created[3]) || !contains(delta, fresh) {
		t.Fatalf("delta: %v", delta)
	}
	if vers[created[3]] < 2 {
		t.Fatalf("the voided row carries its new version: %v", vers)
	}
	// Account balance changes arrive through the accounts feed.
	accs, _, _, _ := pullAll(t, e, tok, "accounts", "", 10)
	if len(accs) != 1 || accs[0] != a {
		t.Fatalf("accounts: %v", accs)
	}
	_ = cur2
	// Edits that bypass the application (a manual fix, a future code path) still move updated_at via the trigger.
	time.Sleep(5 * time.Millisecond)
	if _, err := pool.Exec(context.Background(), `UPDATE transactions SET notes='fixed by hand' WHERE id=$1`, created[7]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if d, _, _, _ := pullAll(t, e, tok, "transactions", cur2, 10); len(d) != 1 || d[0] != created[7] {
		t.Fatalf("trigger: %v", d)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestSyncPullHoldsBackRecentRowsAndIsolatesUsers(t *testing.T) {
	e := setup(t) // default 30 second settle window
	tok, _, _ := e.signUp(t, "asha@example.com")
	other, _, _ := e.signUp(t, "ravi@example.com")
	a := e.newAccount(t, tok, "HDFC", "savings", 100000)
	e.spend(t, tok, a, 100, nil)
	if got, _, _, _ := pullAll(t, e, tok, "transactions", "", 10); len(got) != 0 {
		t.Fatalf("a row younger than the settle window must wait, so a slow commit can never land behind the cursor: %v", got)
	}
	settleNow(t)
	if got, _, _, _ := pullAll(t, e, tok, "transactions", "", 10); len(got) != 1 {
		t.Fatalf("after the window: %v", got)
	}
	if got, _, _, _ := pullAll(t, e, other, "transactions", "", 10); len(got) != 0 {
		t.Fatalf("another user's pull leaked: %v", got)
	}
	if got, _, _, _ := pullAll(t, e, other, "accounts", "", 10); len(got) != 0 {
		t.Fatalf("accounts leaked: %v", got)
	}
	for _, c := range []string{"garbage", "e30"} {
		if r := e.do(t, "GET", "/v1/sync/transactions?cursor="+c, tok, nil); r.Status != 400 {
			t.Errorf("cursor %q: %d", c, r.Status)
		}
		if r := e.do(t, "GET", "/v1/sync/accounts?cursor="+c, tok, nil); r.Status != 400 {
			t.Errorf("account cursor %q: %d", c, r.Status)
		}
	}
	if r := e.do(t, "GET", "/v1/sync/transactions", "", nil); r.Status != 401 {
		t.Fatalf("unauthenticated: %d", r.Status)
	}
}

func TestMaintenanceAndUpdateRequired(t *testing.T) {
	e := setupConfigured(t, true, func(c *config.Config) { c.MinClientVersion = "0.3.0" })
	tok, _, _ := e.signUp(t, "asha@example.com")
	get := func(version string) int {
		r, _ := httpNewRequest(e.srv.URL+"/v1/accounts", tok, version)
		return r
	}
	if get("0.2.9") != 426 || get("0.3.0") != 200 || get("1.0.0") != 200 || get("0.3.0-debug") != 200 || get("") != 200 {
		t.Fatal("old builds are refused with 426, current and newer builds and header-less callers pass")
	}
	meta := e.do(t, "GET", "/v1/meta", "", nil)
	if meta.Status != 200 || str(meta.data(), "min_client_version") != "0.3.0" || meta.data()["maintenance"] != false {
		t.Fatalf("meta: %s", meta.Raw)
	}
	if r := e.do(t, "GET", "/healthz", "", nil); r.Status != 200 {
		t.Fatal("health stays reachable")
	}
	m := setupConfigured(t, true, func(c *config.Config) { c.MaintenanceMode = true })
	if r := m.do(t, "POST", "/v1/auth/sign-in", "", map[string]any{"email": "a@b.co", "password": "x"}); r.Status != 503 || r.errCode() != "maintenance" {
		t.Fatalf("maintenance: %d %s", r.Status, r.Raw)
	}
	if r := m.do(t, "GET", "/v1/meta", "", nil); r.Status != 200 || r.data()["maintenance"] != true {
		t.Fatalf("meta during maintenance must still answer: %s", r.Raw)
	}
}

func httpNewRequest(url, token, version string) (int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if version != "" {
		req.Header.Set("X-Client-Version", version)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}
