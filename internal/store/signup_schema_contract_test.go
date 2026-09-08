package store

import (
	"os"
	"strings"
	"testing"
)

func TestSelfSignupSchemaKeepsPrivilegeAndRaceGuards(t *testing.T) {
	body, err := os.ReadFile("../../migrations/0010_self_signup.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)

	dropOld := strings.Index(sql, "DROP CONSTRAINT %I")
	addNew := strings.Index(sql, "ADD CONSTRAINT users_role_tenant_scope")
	if dropOld < 0 || addNew < 0 || dropOld > addNew {
		t.Fatal("the platform-admin-only tenant constraint must be dropped before the wider one is added")
	}

	createStart := strings.Index(sql, "CREATE FUNCTION public.signup_create_account")
	if createStart < 0 {
		t.Fatal("signup function is missing")
	}
	create := sql[createStart:]
	lock := strings.Index(create, "pg_advisory_xact_lock")
	existing := strings.Index(create, "IF EXISTS (SELECT 1 FROM public.users WHERE lower(email) = v_email)")
	insert := strings.Index(create, "INSERT INTO public.users")
	if lock < 0 || existing < 0 || insert < 0 || lock > existing || existing > insert {
		t.Fatal("signup must lock the email, reject an existing account, and only then insert")
	}
	pending := strings.Index(create, "FROM public.invitations")
	if pending < 0 || pending > insert {
		t.Fatal("signup must reject a pending invitation before creating the account")
	}
	if strings.Contains(create[:insert+400], "'platform_admin'") || strings.Contains(create[:insert+400], "'tenant_admin'") {
		t.Fatal("signup must never grant an administrator role")
	}

	assignStart := strings.Index(sql, "CREATE FUNCTION public.signup_set_account_tenant")
	if assignStart < 0 {
		t.Fatal("assignment function is missing")
	}
	assign := sql[assignStart:]
	actorCheck := strings.Index(assign, "platform administrator role is required")
	update := strings.Index(assign, "UPDATE public.users SET tenant_id = p_tenant_id")
	memberOnly := strings.Index(assign, "WHERE id = p_user_id AND role = 'member'")
	if actorCheck < 0 || update < 0 || memberOnly < 0 || actorCheck > update || update > memberOnly {
		t.Fatal("assignment must verify the platform administrator before updating one member account")
	}

	for _, contract := range []string{
		"SECURITY DEFINER",
		"SET search_path = pg_catalog",
		"BYPASSRLS NOINHERIT",
		"GRANT SELECT, INSERT, UPDATE ON TABLE public.users TO namo_signup_definer",
		"ALTER FUNCTION public.signup_create_account(text, text) OWNER TO namo_signup_definer",
		"ALTER FUNCTION public.signup_member_accounts(uuid) OWNER TO namo_signup_definer",
		"ALTER FUNCTION public.signup_set_account_tenant(uuid, uuid, uuid) OWNER TO namo_signup_definer",
		"REVOKE ALL ON FUNCTION public.signup_member_accounts(uuid) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.signup_set_account_tenant(uuid, uuid, uuid) FROM PUBLIC",
		"GRANT EXECUTE ON FUNCTION public.signup_set_account_tenant(uuid, uuid, uuid) TO namo_runtime",
	} {
		if !strings.Contains(sql, contract) {
			t.Fatalf("self signup migration missing contract: %s", contract)
		}
	}
	if strings.Count(sql, "SECURITY DEFINER") != 3 {
		t.Fatalf("SECURITY DEFINER count = %d, want one per signup function", strings.Count(sql, "SECURITY DEFINER"))
	}
	for _, forbidden := range []string{
		"DELETE ON TABLE public.users",
		"GRANT ALL",
		"TO PUBLIC",
		"public.sessions",
	} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("self signup migration contains forbidden grant: %s", forbidden)
		}
	}
}

func TestSignupQualificationSchemaKeepsGuardsAfterReplacement(t *testing.T) {
	body, err := os.ReadFile("../../migrations/0011_signup_column_qualification.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)

	for _, function := range []string{
		"public.signup_create_account",
		"public.signup_member_accounts",
		"public.signup_set_account_tenant",
	} {
		if !strings.Contains(sql, "CREATE OR REPLACE FUNCTION "+function) {
			t.Fatalf("replacement is missing for %s", function)
		}
	}
	if strings.Count(sql, "SECURITY DEFINER") != 3 || strings.Count(sql, "SET search_path = pg_catalog") != 3 {
		t.Fatal("each replaced function must stay SECURITY DEFINER with a fixed search_path")
	}

	createStart := strings.Index(sql, "CREATE OR REPLACE FUNCTION public.signup_create_account")
	create := sql[createStart:strings.Index(sql, "CREATE OR REPLACE FUNCTION public.signup_member_accounts")]
	lock := strings.Index(create, "pg_advisory_xact_lock")
	existing := strings.Index(create, "WHERE lower(existing.email) = v_email")
	pending := strings.Index(create, "WHERE lower(pending.email) = v_email")
	insert := strings.Index(create, "INSERT INTO public.users")
	if lock < 0 || existing < 0 || pending < 0 || insert < 0 || lock > existing || existing > pending || pending > insert {
		t.Fatal("signup must lock the email, reject an account and a pending invitation, and only then insert")
	}
	if !strings.Contains(create, "VALUES (NULL, v_email, v_display_name, p_password_hash, 'member')") {
		t.Fatal("signup must still create a member without a tenant")
	}

	assign := sql[strings.Index(sql, "CREATE OR REPLACE FUNCTION public.signup_set_account_tenant"):]
	actor := strings.Index(assign, "platform administrator role is required")
	update := strings.Index(assign, "UPDATE public.users member SET tenant_id = p_tenant_id")
	memberOnly := strings.Index(assign, "WHERE member.id = p_user_id AND member.role = 'member'")
	if actor < 0 || update < 0 || memberOnly < 0 || actor > update || update > memberOnly {
		t.Fatal("assignment must verify the platform administrator before updating one member account")
	}
}

func TestTenantRegistrySchemaRegistersWithoutInvitation(t *testing.T) {
	body, err := os.ReadFile("../../migrations/0012_admin_tenant_registry.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)

	register := sql[strings.Index(sql, "CREATE FUNCTION public.admin_register_tenant"):strings.Index(sql, "CREATE FUNCTION public.admin_tenant_registry")]
	actor := strings.Index(register, "platform administrator role is required")
	lock := strings.Index(register, "pg_advisory_xact_lock")
	duplicate := strings.Index(register, "tenant is already registered")
	insert := strings.Index(register, "INSERT INTO public.tenants")
	schedule := strings.Index(register, "INSERT INTO public.schedules")
	if actor < 0 || lock < 0 || duplicate < 0 || insert < 0 || schedule < 0 ||
		actor > lock || lock > duplicate || duplicate > insert || insert > schedule {
		t.Fatal("registration must check the actor, lock the name, reject a duplicate, then create the tenant and its schedule")
	}
	if strings.Contains(sql, "public.invitations") || strings.Contains(sql, "INSERT INTO public.users") {
		t.Fatal("registration must not create invitations or accounts")
	}

	registry := sql[strings.Index(sql, "CREATE FUNCTION public.admin_tenant_registry"):]
	if !strings.Contains(registry, "#variable_conflict use_column") {
		t.Fatal("the registry listing must state its variable conflict resolution")
	}
	for _, qualified := range []string{
		"FROM public.tenants company",
		"SELECT company.id, company.name, company.contact_email, company.admin_name, company.admin_email",
		"WHERE seat.tenant_id = company.id",
		"ORDER BY company.created_at, company.id",
	} {
		if !strings.Contains(registry, qualified) {
			t.Fatalf("registry listing must keep qualified references: %s", qualified)
		}
	}
	if strings.Count(sql, "SECURITY DEFINER") != 2 || strings.Count(sql, "SET search_path = pg_catalog") != 2 {
		t.Fatal("both registry functions must be SECURITY DEFINER with a fixed search_path")
	}
	for _, contract := range []string{
		"ALTER FUNCTION public.admin_register_tenant(uuid, text, text, text, text) OWNER TO namo_signup_definer",
		"REVOKE ALL ON FUNCTION public.admin_register_tenant(uuid, text, text, text, text) FROM PUBLIC",
		"REVOKE ALL ON FUNCTION public.admin_tenant_registry(uuid) FROM PUBLIC",
	} {
		if !strings.Contains(sql, contract) {
			t.Fatalf("tenant registry migration missing contract: %s", contract)
		}
	}
}

func TestAccountAccessSchemaGrantsCompanyRoleSafely(t *testing.T) {
	body, err := os.ReadFile("../../migrations/0013_admin_account_access.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)

	// The superseded single-purpose functions must not stay callable.
	for _, dropped := range []string{
		"DROP FUNCTION public.signup_set_account_tenant(uuid, uuid, uuid)",
		"DROP FUNCTION public.signup_member_accounts(uuid)",
	} {
		if !strings.Contains(sql, dropped) {
			t.Fatalf("migration must drop the superseded function: %s", dropped)
		}
	}

	access := sql[strings.Index(sql, "CREATE FUNCTION public.admin_set_account_access"):]
	actor := strings.Index(access, "platform administrator role is required")
	roleCheck := strings.Index(access, "IF v_role NOT IN ('member', 'tenant_admin') THEN")
	companyCheck := strings.Index(access, "IF p_tenant_id IS NULL AND v_role <> 'member' THEN")
	tenantCheck := strings.Index(access, "tenant is unavailable")
	update := strings.Index(access, "UPDATE public.users seat SET tenant_id = p_tenant_id, role = v_role")
	targetGuard := strings.Index(access, "WHERE seat.id = p_user_id AND seat.role IN ('member', 'tenant_admin')")
	if actor < 0 || roleCheck < 0 || companyCheck < 0 || tenantCheck < 0 || update < 0 || targetGuard < 0 ||
		actor > roleCheck || roleCheck > companyCheck || companyCheck > tenantCheck ||
		tenantCheck > update || update > targetGuard {
		t.Fatal("access changes must verify actor, role, company pairing and target before the update")
	}
	// The only platform_admin mention may be the actor check; the target and
	// the granted role must stay inside the company roles.
	if strings.Count(access, "'platform_admin'") != 1 || strings.Index(access, "'platform_admin'") > roleCheck {
		t.Fatal("company access must never grant or target the platform administrator role")
	}

	registry := sql[strings.Index(sql, "CREATE FUNCTION public.admin_account_registry"):strings.Index(sql, "CREATE FUNCTION public.admin_set_account_access")]
	if !strings.Contains(registry, "#variable_conflict use_column") {
		t.Fatal("the account listing must state its variable conflict resolution")
	}
	for _, qualified := range []string{
		"FROM public.users seat",
		"LEFT JOIN public.tenants company ON company.id = seat.tenant_id",
		"WHERE seat.role IN ('member', 'tenant_admin')",
		"ORDER BY (seat.tenant_id IS NOT NULL), seat.created_at DESC, seat.id",
	} {
		if !strings.Contains(registry, qualified) {
			t.Fatalf("account listing must keep qualified references: %s", qualified)
		}
	}
	if strings.Count(sql, "SECURITY DEFINER") != 2 || strings.Count(sql, "SET search_path = pg_catalog") != 2 {
		t.Fatal("both access functions must be SECURITY DEFINER with a fixed search_path")
	}
}

func TestAccountRemovalSchemaSeparatesAuthority(t *testing.T) {
	body, err := os.ReadFile("../../migrations/0014_account_removal.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)

	remove := sql[strings.Index(sql, "CREATE FUNCTION public.tenant_remove_member"):strings.Index(sql, "CREATE FUNCTION public.admin_delete_account")]
	actor := strings.Index(remove, "company administrator role is required")
	self := strings.Index(remove, "an administrator cannot remove itself")
	update := strings.Index(remove, "UPDATE public.users seat SET tenant_id = NULL, role = 'member'")
	scope := strings.Index(remove, "AND seat.tenant_id = p_tenant_id")
	if actor < 0 || self < 0 || update < 0 || scope < 0 || actor > self || self > update || update > scope {
		t.Fatal("company removal must verify the administrator, refuse itself, then unassign inside its own company")
	}
	if strings.Contains(remove, "DELETE FROM") {
		t.Fatal("company removal must keep the account")
	}

	delete := sql[strings.Index(sql, "CREATE FUNCTION public.admin_delete_account"):]
	platform := strings.Index(delete, "platform administrator role is required")
	selfDelete := strings.Index(delete, "an administrator cannot delete itself")
	statement := strings.Index(delete, "DELETE FROM public.users seat")
	guard := strings.Index(delete, "WHERE seat.id = p_user_id AND seat.role IN ('member', 'tenant_admin')")
	if platform < 0 || selfDelete < 0 || statement < 0 || guard < 0 ||
		platform > selfDelete || selfDelete > statement || statement > guard {
		t.Fatal("account deletion must verify the platform administrator, refuse itself, then delete one company account")
	}
	if !strings.Contains(sql, "GRANT DELETE ON TABLE public.users TO namo_signup_definer") {
		t.Fatal("deletion needs the DELETE grant on users")
	}
	for _, forbidden := range []string{"DELETE FROM public.tenants", "GRANT DELETE ON TABLE public.tenants", "TRUNCATE"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("removal migration reaches beyond one account: %s", forbidden)
		}
	}
	if strings.Count(sql, "SECURITY DEFINER") != 2 || strings.Count(sql, "SET search_path = pg_catalog") != 2 {
		t.Fatal("both removal functions must be SECURITY DEFINER with a fixed search_path")
	}
}
