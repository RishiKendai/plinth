package app_test

import "testing"

func TestCategoriesArePrivateToTheirOwnerAndArchivingCascades(t *testing.T) {
	e := setup(t)
	ta, _, _ := e.signUp(t, "asha@example.com")
	tb, _, _ := e.signUp(t, "ravi@example.com")
	parent := e.call(t, "POST", "/v1/categories", ta, "", map[string]any{"name": "Pets", "kind": "expense"})
	if parent.Status != 201 {
		t.Fatalf("create: %d %s", parent.Status, parent.Raw)
	}
	pid := str(parent.data(), "id")
	sub := e.call(t, "POST", "/v1/categories", ta, "", map[string]any{"name": "Vet", "kind": "expense", "parent_id": pid})
	if sub.Status != 201 {
		t.Fatalf("sub: %d %s", sub.Status, sub.Raw)
	}
	sid := str(sub.data(), "id")
	has := func(tok, id string) bool {
		for _, c := range e.do(t, "GET", "/v1/categories", tok, nil).Body["data"].([]any) {
			if str(c.(map[string]any), "id") == id {
				return true
			}
		}
		return false
	}
	if !has(ta, pid) || !has(ta, sid) {
		t.Fatal("the owner sees their category and subcategory")
	}
	if has(tb, pid) || has(tb, sid) {
		t.Fatal("another user must not see them")
	}
	if r := e.call(t, "PUT", "/v1/categories/"+pid, tb, "", map[string]any{"name": "Mine now"}); r.Status != 404 {
		t.Fatalf("another user editing: %d", r.Status)
	}
	if r := e.call(t, "PUT", "/v1/categories/"+pid, ta, "", map[string]any{"archived": true}); r.Status != 200 {
		t.Fatalf("archive: %d %s", r.Status, r.Raw)
	}
	if has(ta, pid) || has(ta, sid) {
		t.Fatal("archiving a category hides its subcategories too")
	}
}
