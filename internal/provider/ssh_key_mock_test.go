package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gjabell/terraform-provider-borgbase/gql"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// PublicSSHKeyType fields and sshAdd/sshDelete arguments as reported by the
// live BorgBase GraphQL schema (introspection, 2026-11).
var (
	sshKeyTypeFields = setOf("id", "name", "keyData", "hashMd5", "hashSha256",
		"keyType", "bits", "addedAt", "lastUsedAt", "keyAdded")
	sshMutationArgs = map[string]map[string]bool{
		"sshAdd":    setOf("name", "keyData"),
		"sshDelete": setOf("id"),
	}
)

type mockSshBorgBase struct {
	t    *testing.T
	keys []map[string]interface{}
	ops  []string
	vars []map[string]interface{}
}

func newSshMock(t *testing.T) (*mockSshBorgBase, *gql.Client) {
	m := &mockSshBorgBase{t: t}
	server := httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(server.Close)
	return m, gql.NewClient(server.URL, "")
}

func (m *mockSshBorgBase) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}
	_ = json.Unmarshal(body, &req)
	reply := func(data interface{}, msg string) {
		out := map[string]interface{}{}
		if msg != "" {
			out["errors"] = []map[string]interface{}{{"message": msg}}
		} else {
			out["data"] = data
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	open := strings.Index(req.Query, "{")
	rest := req.Query[open+1:]
	field := strings.SplitN(strings.TrimSpace(rest), "(", 2)[0]
	field = strings.TrimSpace(strings.SplitN(field, "{", 2)[0])
	if field != "sshDelete" {
		selection := rest[strings.LastIndex(rest, "(")+1:]
		selection = selection[strings.Index(selection, "{")+1:]
		for _, f := range regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`).FindAllString(selection, -1) {
			if !sshKeyTypeFields[f] {
				reply(nil, `Cannot query field "`+f+`" on type "PublicSSHKeyType".`)
				return
			}
		}
	}
	if allowed, ok := sshMutationArgs[field]; ok {
		for k := range req.Variables {
			if !allowed[k] {
				reply(nil, `Unknown argument "`+k+`" on field "`+field+`".`)
				return
			}
			// All sshAdd/sshDelete arguments are String! in the live schema.
			if !strings.Contains(req.Query, "$"+k+": String!") {
				reply(nil, "Variable '$"+k+"' of type 'String' used in position expecting type 'String!'.")
				return
			}
		}
	}
	m.ops = append(m.ops, field)
	m.vars = append(m.vars, req.Variables)
	switch field {
	case "sshList":
		var out []map[string]interface{}
		for _, k := range m.keys {
			if n, ok := req.Variables["name"].(string); !ok || n == k["name"] {
				out = append(out, k)
			}
		}
		reply(map[string]interface{}{"sshList": out}, "")
	case "sshAdd":
		key := map[string]interface{}{
			"id": "42", "name": req.Variables["name"],
			"keyData": strings.Join(strings.Fields(req.Variables["keyData"].(string))[:2], " "),
			"hashMd5": "aa:bb", "hashSha256": "abc", "keyType": "ssh-ed25519",
			"bits": 256, "addedAt": "2026-11-01T00:00:00+00:00", "lastUsedAt": nil,
		}
		m.keys = append(m.keys, key)
		reply(map[string]interface{}{"sshAdd": map[string]interface{}{"keyAdded": key}}, "")
	case "sshDelete":
		m.keys = nil
		reply(map[string]interface{}{"sshDelete": map[string]interface{}{"ok": true}}, "")
	default:
		reply(nil, "unsupported "+field)
	}
}

func sshModel(name, key string) SshKeyModel {
	return SshKeyModel{
		AddedAt: types.StringUnknown(), Bits: types.Int64Unknown(),
		HashMd5: types.StringUnknown(), HashSha256: types.StringUnknown(),
		Id: types.StringUnknown(), LastUsedAt: types.StringUnknown(),
		Name: types.StringValue(name), PublicKey: types.StringValue(key),
		Type: types.StringUnknown(),
	}
}

func sshState(t *testing.T, sr resource.SchemaResponse, m *SshKeyModel) tfsdk.State {
	st := tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(sr.Schema.Type().TerraformType(context.Background()), nil)}
	if m != nil {
		if d := st.Set(context.Background(), m); d.HasError() {
			t.Fatal(d)
		}
	}
	return st
}

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBAt/X37WDQ3cNPEVHQBsW3lH7XPeea5rUoeXuhoTkzR"

func TestSshKeyCreateKeepsConfiguredSpelling(t *testing.T) {
	mock, client := newSshMock(t)
	r := &SshKeyResource{client: client}
	var sr resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)

	configured := testKey + " comment\n"
	plan := sshModel("k3s", configured)
	planState := sshState(t, sr, &plan)
	resp := resource.CreateResponse{State: sshState(t, sr, nil)}
	r.Create(context.Background(), resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sr.Schema, Raw: planState.Raw},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if mock.ops[0] != "sshAdd" || mock.vars[0]["keyData"] != configured || mock.vars[0]["name"] != "k3s" {
		t.Fatalf("unexpected request %v %v", mock.ops, mock.vars)
	}
	var got SshKeyModel
	resp.State.Get(context.Background(), &got)
	if got.Id.ValueString() != "42" || got.PublicKey.ValueString() != configured {
		t.Fatalf("configured key spelling not kept: %+v", got)
	}
	if got.LastUsedAt.ValueString() != "" {
		t.Fatalf("null lastUsedAt should map to empty string")
	}

	// Read keeps the configured spelling, so no replacement is planned.
	state := resp.State
	rr := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &rr)
	if rr.Diagnostics.HasError() {
		t.Fatal(rr.Diagnostics)
	}
	rr.State.Get(context.Background(), &got)
	if got.PublicKey.ValueString() != configured {
		t.Fatalf("read changed public_key: %q", got.PublicKey.ValueString())
	}
}

func TestSshKeyReadMissingRemovesAndDeleteSendsID(t *testing.T) {
	mock, client := newSshMock(t)
	r := &SshKeyResource{client: client}
	var sr resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)

	model := sshModel("gone", testKey)
	model.Id = types.StringValue("7")
	model.AddedAt, model.Bits = types.StringValue(""), types.Int64Value(256)
	model.HashMd5, model.HashSha256 = types.StringValue(""), types.StringValue("")
	model.LastUsedAt, model.Type = types.StringValue(""), types.StringValue("")
	state := sshState(t, sr, &model)

	rr := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &rr)
	if rr.Diagnostics.HasError() || !rr.State.Raw.IsNull() {
		t.Fatalf("expected removal: %v", rr.Diagnostics)
	}

	dr := resource.DeleteResponse{State: state}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, &dr)
	last := len(mock.ops) - 1
	if dr.Diagnostics.HasError() || mock.ops[last] != "sshDelete" || mock.vars[last]["id"] != "7" {
		t.Fatalf("unexpected delete: %v %v %v", mock.ops, mock.vars, dr.Diagnostics)
	}
}

func TestSshKeyImportReadsKeyFromAPI(t *testing.T) {
	mock, client := newSshMock(t)
	mock.keys = []map[string]interface{}{{
		"id": "9", "name": "existing", "keyData": testKey, "hashMd5": "m",
		"hashSha256": "s", "keyType": "ssh-ed25519", "bits": 256,
		"addedAt": "2026-11-01T00:00:00+00:00", "lastUsedAt": nil,
	}}
	r := &SshKeyResource{client: client}
	var sr resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)
	imp := resource.ImportStateResponse{State: sshState(t, sr, nil)}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "existing"}, &imp)
	if imp.Diagnostics.HasError() {
		t.Fatal(imp.Diagnostics)
	}
	var model SshKeyModel
	imp.State.Get(context.Background(), &model)
	rr := resource.ReadResponse{State: imp.State}
	r.Read(context.Background(), resource.ReadRequest{State: imp.State}, &rr)
	if rr.Diagnostics.HasError() {
		t.Fatal(rr.Diagnostics)
	}
	rr.State.Get(context.Background(), &model)
	if model.Id.ValueString() != "9" || model.PublicKey.ValueString() != testKey {
		t.Fatalf("unexpected imported state: %+v", model)
	}
}

func TestSameKey(t *testing.T) {
	for in, want := range map[string]bool{
		testKey + " c\n": true, testKey: true, "ssh-rsa AAAA": false, "": false,
	} {
		if got := sameKey(in, testKey); got != want {
			t.Errorf("sameKey(%q) = %v, want %v", in, got, want)
		}
	}
}
