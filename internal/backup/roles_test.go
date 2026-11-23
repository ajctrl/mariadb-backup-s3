package backup

import "testing"

func TestRoleGrantScope(t *testing.T) {
	for _, tc := range []struct {
		grant, role, db, table string
		hasTrigger, keep       bool
	}{
		{"GRANT USAGE ON *.* TO `analyst`", "", "", "", false, false},
		{"GRANT ALL PRIVILEGES ON *.* TO `operator` WITH GRANT OPTION", "operator", "*", "*", true, true},
		{"GRANT `TRIGGER` TO `operator`", "", "", "", false, false},
		{"GRANT SELECT, TRIGGER ON `data&archive`.* TO PUBLIC", "PUBLIC", "data&archive", "*", true, true},
		{"GRANT SELECT ON `data\\_%`.* TO `audit`", "audit", "data\\_%", "*", false, true},
		{"GRANT SELECT (`x, TRIGGER, y`) ON `app`.`orders` TO `audit`", "", "", "", false, false},
		{"GRANT SELECT (` ON ``db``.``table`` TO PUBLIC`), TRIGGER ON `a.b`.`t``2` TO `role``1`", "role`1", "a.b", "t`2", true, true},
		{"GRANT ALL PRIVILEGES ON PROCEDURE `app`.`hook` TO PUBLIC", "", "", "", false, false},
	} {
		g, ok := parseRoleGrant(tc.grant)
		if ok != tc.keep {
			t.Errorf("%q: keep=%t", tc.grant, ok)
			continue
		}
		if !ok {
			continue
		}
		scope := func(name *string) string {
			if name == nil {
				return "*"
			}
			return *name
		}
		if g.Role != tc.role || scope(g.DB) != tc.db || scope(g.Tbl) != tc.table || g.Trigger != tc.hasTrigger {
			t.Errorf("%q: role=%q db=%q table=%q trigger=%t", tc.grant, g.Role, scope(g.DB), scope(g.Tbl), g.Trigger)
		}
	}
}
