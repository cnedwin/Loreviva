package contracts

import (
 "testing"
 "loreviva.local/loreviva/internal/generated"
)

func TestStudioGeneratedValidation(t *testing.T) {
 for _, tc := range []struct{name,body string; valid bool}{
  {"StudioSettingsScope",`{"scope_type":"TENANT","tenant_id":"demo-local"}`,true},
  {"StudioSettingsScope",`{"scope_type":"TENANT","tenant_id":"demo-local","product_id":"fake"}`,false},
  {"DraftCandidateArtifact",`{"fields":{"example_lines":["one","two"]}}`,true},
  {"DraftCandidateArtifact",`{"fields":{"example_lines":"wrong"}}`,false},
  {"DraftCandidateArtifact",`{"fields":{"description":"first","description":"second"}}`,false},
  {"RehearsalReportArtifact",`{"program":"R0_REHEARSAL_REPORT_V1","inputs":[],"assertions":[{"kind":"RIGHTS","state":"FAIL"},{"kind":"RIGHTS","state":"NOT_RUN"}]}`,true},
 } {
  if err:=generated.Validate(tc.name,[]byte(tc.body));(err==nil)!=tc.valid {t.Errorf("%s valid=%v err=%v",tc.name,tc.valid,err)}
 }
}
