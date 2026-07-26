package access

import "testing"

func TestPolicyIdentitiesExtractsOnlySupportedIncludeRules(t *testing.T) {
	emails, domains := policyIdentities([]interface{}{
		map[string]interface{}{"email": map[string]interface{}{"email": "person@example.com"}},
		map[string]interface{}{"email_domain": map[string]interface{}{"domain": "example.com"}},
		map[string]interface{}{"ip": map[string]interface{}{"ip": "192.0.2.1"}},
	})
	if len(emails) != 1 || emails[0] != "person@example.com" || len(domains) != 1 || domains[0] != "example.com" {
		t.Fatalf("emails=%#v domains=%#v", emails, domains)
	}
}
