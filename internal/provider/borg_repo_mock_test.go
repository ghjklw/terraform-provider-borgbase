package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gjabell/terraform-provider-borgbase/gql"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Fields of RepoType and arguments of the repo mutations, as reported by the
// live BorgBase GraphQL schema (introspection, 2026-10).
var (
	repoTypeFields = setOf(
		"id", "name", "region", "borgVersion", "quota", "quotaEnabled",
		"lastModified", "createdAt", "alertDays", "accessMode", "sftpEnabled",
		"appendOnly", "format", "compactionEnabled", "compactionInterval",
		"compactionIntervalUnit", "compactionHour", "compactionHourTimezone",
		"htpasswd", "vykarToken", "encryption", "server", "repoPath",
		"currentUsage", "appendOnlyKeys", "fullAccessKeys", "appendOnlyKeyList",
		"fullAccessKeyList",
		// ServerType
		"hostname", "public", "location", "fingerprintRsa",
		"fingerprintEcdsa", "fingerprintEd25519",
		// mutation payload wrappers
		"repoAdded", "repoEdited",
	)
	mutationArgs = map[string]map[string]bool{
		"repoAdd": setOf("name", "region", "borgVersion", "quota",
			"quotaEnabled", "alertDays", "appendOnlyKeys", "fullAccessKeys",
			"accessMode", "sftpEnabled", "appendOnly", "format",
			"compactionEnabled", "compactionInterval",
			"compactionIntervalUnit", "compactionHour",
			"compactionHourTimezone"),
		"repoEdit": setOf("id", "name", "region", "borgVersion", "quota",
			"quotaEnabled", "alertDays", "appendOnlyKeys", "fullAccessKeys",
			"accessMode", "sftpEnabled", "appendOnly", "compactionEnabled",
			"compactionInterval", "compactionIntervalUnit", "compactionHour",
			"compactionHourTimezone"),
	}
)

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

type recordedRequest struct {
	Operation string
	Query     string
	Variables map[string]interface{}
}

type mockBorgBase struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	reqs   []recordedRequest
	repos  []map[string]interface{}
}

func fixtureRepo() map[string]interface{} {
	return map[string]interface{}{
		"id": "abc123", "name": "backup", "region": "eu",
		"server": map[string]interface{}{
			"id": "srv1", "hostname": "h.repo.borgbase.com", "region": "eu",
			"public": true, "location": "Frankfurt",
			"fingerprintRsa": "r", "fingerprintEcdsa": "e", "fingerprintEd25519": "d",
		},
		"quota": 0, "quotaEnabled": false, "alertDays": 0, "format": "borg1",
		"borgVersion": "LATEST", "accessMode": "fullAccess",
		"appendOnly": true, "appendOnlyKeys": []string{"k1"},
		"fullAccessKeys": []string{"k2"}, "sftpEnabled": false,
		"encryption": "repokey", "createdAt": "2024-01-01T00:00:00+00:00",
		"lastModified": nil, "compactionEnabled": true,
		"compactionInterval": 6, "compactionIntervalUnit": "weeks",
		"compactionHour": 14, "compactionHourTimezone": "UTC",
		"repoPath": "ssh://x@h.repo.borgbase.com/./repo", "currentUsage": 12.5,
	}
}

var selectionRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

func newMock(t *testing.T) *mockBorgBase {
	m := &mockBorgBase{t: t, repos: []map[string]interface{}{fixtureRepo()}}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockBorgBase) client() *gql.Client {
	return gql.NewClient(m.server.URL, "")
}

func (m *mockBorgBase) last() recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reqs) == 0 {
		m.t.Fatal("no request recorded")
	}
	return m.reqs[len(m.reqs)-1]
}

func (m *mockBorgBase) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Query     string          `json:"query"`
		Variables json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	vars := map[string]interface{}{}
	raw := req.Variables
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		raw = json.RawMessage(asString)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &vars); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	}

	// Validate the selection set against RepoType: unknown fields produce a
	// GraphQL error, as the live API does.
	open := strings.Index(req.Query, "{")
	header := req.Query[:open]
	op := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(header, "query "), "mutation "))[0]
	op = strings.SplitN(op, "(", 2)[0]
	rest := req.Query[open+1:]
	field := strings.SplitN(strings.TrimSpace(rest), "(", 2)[0]
	selection := rest[strings.LastIndex(rest, "(")+1:]
	selection = selection[strings.Index(selection, "{")+1:]
	var bad []string
	for _, f := range selectionRe.FindAllString(selection, -1) {
		if !repoTypeFields[f] {
			bad = append(bad, f)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		m.reply(w, nil, `Cannot query field "`+bad[0]+`" on type "RepoType".`)
		return
	}
	if allowed, ok := mutationArgs[field]; ok {
		for k := range vars {
			if !allowed[k] {
				m.reply(w, nil, `Unknown argument "`+k+`" on field "`+field+`".`)
				return
			}
		}
	}

	m.mu.Lock()
	m.reqs = append(m.reqs, recordedRequest{Operation: field, Query: req.Query, Variables: vars})
	m.mu.Unlock()

	switch field {
	case "repoList":
		var out []map[string]interface{}
		for _, repo := range m.repos {
			if n, ok := vars["name"].(string); !ok || n == repo["name"] {
				out = append(out, repo)
			}
		}
		m.reply(w, map[string]interface{}{"repoList": out}, "")
	case "repoEdit":
		repo := m.repos[0]
		for k, v := range vars {
			if k != "id" {
				repo[k] = v
			}
		}
		m.reply(w, map[string]interface{}{"repoEdit": map[string]interface{}{"repoEdited": repo}}, "")
	case "repoAdd":
		repo := fixtureRepo()
		for k, v := range vars {
			repo[k] = v
		}
		m.reply(w, map[string]interface{}{"repoAdd": map[string]interface{}{"repoAdded": repo}}, "")
	default:
		m.reply(w, nil, "unsupported "+op)
	}
}

func (m *mockBorgBase) reply(w http.ResponseWriter, data interface{}, errMsg string) {
	out := map[string]interface{}{}
	if errMsg != "" {
		out["errors"] = []map[string]interface{}{{"message": errMsg}}
	} else {
		out["data"] = data
	}
	_ = json.NewEncoder(w).Encode(out)
}

// helpers to build framework state/plan objects

func repoSchema(t *testing.T, r *BorgRepoResource) resource.SchemaResponse {
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp
}

func stateFrom(t *testing.T, sr resource.SchemaResponse, model BorgRepoModel) tfsdk.State {
	st := tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(sr.Schema.Type().TerraformType(context.Background()), nil)}
	if d := st.Set(context.Background(), &model); d.HasError() {
		t.Fatal(d)
	}
	return st
}

func nullModel() BorgRepoModel {
	return BorgRepoModel{
		AccessMode:     types.StringNull(),
		AlertDays:      types.Int64Null(),
		AppendOnly:     types.BoolNull(),
		AppendOnlyKeys: types.ListNull(types.StringType),
		BorgVersion:    types.StringNull(),
		Compaction:     types.ObjectNull(compactionAttributes),
		CreatedAt:      types.StringNull(),
		CurrentUsage:   types.Float64Null(),
		Encryption:     types.StringNull(),
		Format:         types.StringNull(),
		FullAccessKeys: types.ListNull(types.StringType),
		Id:             types.StringNull(),
		LastModified:   types.StringNull(),
		Name:           types.StringNull(),
		Quota:          types.Int64Null(),
		QuotaEnabled:   types.BoolNull(),
		Region:         types.StringNull(),
		RepoPath:       types.StringNull(),
		Server:         types.ObjectNull(serverAttributes),
		SftpEnabled:    types.BoolNull(),
	}
}

func withName(n string) BorgRepoModel {
	m := nullModel()
	m.Name = types.StringValue(n)
	return m
}

func existingModel(t *testing.T) BorgRepoModel {
	m := nullModel()
	repoJSON, _ := json.Marshal(fixtureRepo())
	var payload BorgRepoPayload
	if err := json.Unmarshal(repoJSON, &payload); err != nil {
		t.Fatal(err)
	}
	if d := m.update(context.Background(), payload); d.HasError() {
		t.Fatal(d)
	}
	return m
}

func TestReadByName(t *testing.T) {
	mock := newMock(t)
	r := &BorgRepoResource{client: mock.client()}
	sr := repoSchema(t, r)

	state := stateFrom(t, sr, withName("backup"))
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var got BorgRepoModel
	resp.State.Get(context.Background(), &got)
	if got.Id.ValueString() != "abc123" || got.AccessMode.ValueString() != "fullAccess" {
		t.Fatalf("unexpected state: %+v", got)
	}
	if got.LastModified.ValueString() != "" {
		t.Fatalf("null lastModified should map to empty string")
	}
}

func TestReadMissingRemovesResource(t *testing.T) {
	mock := newMock(t)
	r := &BorgRepoResource{client: mock.client()}
	sr := repoSchema(t, r)

	state := stateFrom(t, sr, withName("gone"))
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Fatal("expected resource to be removed from state")
	}
}

func TestImportStateByName(t *testing.T) {
	r := &BorgRepoResource{}
	sr := repoSchema(t, r)
	resp := resource.ImportStateResponse{State: tfsdk.State{
		Schema: sr.Schema,
		Raw:    tftypes.NewValue(sr.Schema.Type().TerraformType(context.Background()), nil),
	}}
	r.ImportState(context.Background(), resource.ImportStateRequest{ID: "backup"}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var got BorgRepoModel
	resp.State.Get(context.Background(), &got)
	if got.Name.ValueString() != "backup" {
		t.Fatalf("name not set from import id: %+v", got)
	}
}

func TestUpdateSendsOnlyChangedArguments(t *testing.T) {
	mock := newMock(t)
	r := &BorgRepoResource{client: mock.client()}
	sr := repoSchema(t, r)

	prior := existingModel(t)
	state := stateFrom(t, sr, prior)

	// Plan: alert_days changed, volatile computed attributes unknown.
	plan := prior
	plan.AlertDays = types.Int64Value(2)
	plan.LastModified = types.StringUnknown()
	plan.CurrentUsage = types.Float64Unknown()
	planState := stateFrom(t, sr, plan)

	resp := resource.UpdateResponse{State: state}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sr.Schema, Raw: planState.Raw},
		State: state,
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}

	req := mock.last()
	if req.Operation != "repoEdit" {
		t.Fatalf("unexpected operation %s", req.Operation)
	}
	want := map[string]interface{}{"id": "abc123", "alertDays": float64(2)}
	if len(req.Variables) != len(want) {
		t.Fatalf("variables = %v, want only %v", req.Variables, want)
	}
	for k, v := range want {
		if req.Variables[k] != v {
			t.Fatalf("variables = %v, want %v", req.Variables, want)
		}
	}
	var got BorgRepoModel
	resp.State.Get(context.Background(), &got)
	if got.AlertDays.ValueInt64() != 2 || !got.AppendOnly.ValueBool() {
		t.Fatalf("unexpected state after update: %+v", got)
	}
}

func TestUpdateChangedCompactionField(t *testing.T) {
	mock := newMock(t)
	r := &BorgRepoResource{client: mock.client()}
	sr := repoSchema(t, r)

	prior := existingModel(t)
	state := stateFrom(t, sr, prior)

	plan := prior
	var c CompactionModel
	prior.Compaction.As(context.Background(), &c, basetypes.ObjectAsOptions{})
	c.Hour = types.Int64Value(3)
	plan.Compaction, _ = types.ObjectValueFrom(context.Background(), compactionAttributes, c)
	planState := stateFrom(t, sr, plan)

	resp := resource.UpdateResponse{State: state}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sr.Schema, Raw: planState.Raw},
		State: state,
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	req := mock.last()
	if len(req.Variables) != 2 || req.Variables["compactionHour"] != float64(3) {
		t.Fatalf("variables = %v", req.Variables)
	}
}

func TestCreateSendsOnlyConfiguredArguments(t *testing.T) {
	mock := newMock(t)
	r := &BorgRepoResource{client: mock.client()}
	sr := repoSchema(t, r)

	unknownList := types.ListUnknown(types.StringType)
	plan := BorgRepoModel{
		Name:           types.StringValue("new"),
		Region:         types.StringValue("eu"),
		AlertDays:      types.Int64Value(2),
		AppendOnly:     types.BoolUnknown(),
		AppendOnlyKeys: unknownList,
		FullAccessKeys: unknownList,
		BorgVersion:    types.StringUnknown(),
		AccessMode:     types.StringUnknown(),
		Quota:          types.Int64Unknown(),
		QuotaEnabled:   types.BoolUnknown(),
		SftpEnabled:    types.BoolUnknown(),
		Compaction:     types.ObjectUnknown(compactionAttributes),
		CreatedAt:      types.StringUnknown(),
		CurrentUsage:   types.Float64Unknown(),
		Encryption:     types.StringUnknown(),
		Format:         types.StringUnknown(),
		Id:             types.StringUnknown(),
		LastModified:   types.StringUnknown(),
		RepoPath:       types.StringUnknown(),
		Server:         types.ObjectUnknown(serverAttributes),
	}
	planState := stateFrom(t, sr, plan)
	resp := resource.CreateResponse{State: tfsdk.State{
		Schema: sr.Schema,
		Raw:    tftypes.NewValue(sr.Schema.Type().TerraformType(context.Background()), nil),
	}}
	r.Create(context.Background(), resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sr.Schema, Raw: planState.Raw},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	req := mock.last()
	if req.Operation != "repoAdd" || len(req.Variables) != 3 ||
		req.Variables["name"] != "new" || req.Variables["region"] != "eu" ||
		req.Variables["alertDays"] != float64(2) {
		t.Fatalf("unexpected request: %+v", req)
	}
}

func TestHasChange(t *testing.T) {
	a, b := types.Int64Value(1), types.Int64Value(2)
	for name, tc := range map[string]struct {
		plan, state types.Int64
		hasState    bool
		want        bool
	}{
		"null":      {types.Int64Null(), b, true, false},
		"unknown":   {types.Int64Unknown(), b, true, false},
		"same":      {a, a, true, false},
		"different": {a, b, true, true},
		"create":    {a, types.Int64Null(), false, true},
	} {
		if got := hasChange(tc.plan, tc.state, tc.hasState); got != tc.want {
			t.Errorf("%s: got %v want %v", name, got, tc.want)
		}
	}
}
