package migrations

import (
	"strings"
	"testing"
)

func TestAccountRemovalUpgradeLocksBothUsersBeforeCheckingAuthority(t *testing.T) {
	assets, err := All()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, asset := range assets {
		if asset.Version == 17 {
			sql = asset.SQL
		}
	}
	if sql == "" {
		t.Fatal("account-removal concurrency upgrade is missing")
	}
	lock := strings.Index(sql, "ORDER BY seat.id FOR UPDATE")
	authority := strings.Index(sql, "IF NOT EXISTS (")
	update := strings.Index(sql, "UPDATE public.users seat SET tenant_id = NULL, role = 'member'")
	if lock < 0 || authority < lock || update < authority {
		t.Fatal("removal must lock both rows in stable order, recheck authority, then update")
	}
	for _, want := range []string{
		"CREATE OR REPLACE FUNCTION public.tenant_remove_member",
		"seat.id IN (p_actor_user_id, p_user_id)", "seat.tenant_id = p_tenant_id",
		"SECURITY DEFINER", "SET search_path = pg_catalog", "OWNER TO namo_signup_definer",
		"REVOKE ALL ON FUNCTION public.tenant_remove_member(uuid, uuid, uuid) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION public.tenant_remove_member(uuid, uuid, uuid) TO namo_runtime",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("upgrade missing security contract: %s", want)
		}
	}
}
