package provider

import (
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

type SshKeyModel struct {
	AddedAt    types.String `tfsdk:"added_at"`
	Bits       types.Int64  `tfsdk:"bits"`
	HashMd5    types.String `tfsdk:"hash_md5"`
	HashSha256 types.String `tfsdk:"hash_sha256"`
	Id         types.String `tfsdk:"id"`
	LastUsedAt types.String `tfsdk:"last_used_at"`
	Name       types.String `tfsdk:"name"`
	PublicKey  types.String `tfsdk:"public_key"`
	Type       types.String `tfsdk:"type"`
}

// sameKey reports whether two OpenSSH public keys have the same type and key
// data; the optional trailing comment and whitespace are ignored.
func sameKey(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	return len(fa) >= 2 && len(fb) >= 2 && fa[0] == fb[0] && fa[1] == fb[1]
}

func (m *SshKeyModel) update(key SshKeyPayload) {
	m.AddedAt = types.StringValue(key.AddedAt)
	m.Bits = types.Int64Value(int64(key.Bits))
	m.HashMd5 = types.StringValue(key.HashMd5)
	m.HashSha256 = types.StringValue(key.HashSha256)
	m.Id = types.StringValue(key.Id)
	m.LastUsedAt = types.StringValue(key.LastUsedAt)
	m.Name = types.StringValue(key.Name)

	// BorgBase stores no comment. Keep the configured spelling (comment,
	// trailing newline) while it denotes the same key, so that it is neither
	// reported as inconsistent after apply nor as a replacement-forcing change.
	if m.PublicKey.IsNull() || m.PublicKey.IsUnknown() ||
		!sameKey(m.PublicKey.ValueString(), key.KeyData) {
		m.PublicKey = types.StringValue(key.KeyData)
	}

	m.Type = types.StringValue(key.KeyType)
}

type SshKeyPayload struct {
	AddedAt    string `json:"addedAt"`
	Bits       int    `json:"bits"`
	HashMd5    string `json:"hashMd5"`
	HashSha256 string `json:"hashSha256"`
	Id         string `json:"id"`
	LastUsedAt string `json:"lastUsedAt"`
	Name       string `json:"name"`
	KeyData    string `json:"keyData"`
	KeyType    string `json:"keyType"`
}

type SshKeysPayload []SshKeyPayload

type SshAddPayload struct {
	KeyAdded SshKeyPayload `json:"keyAdded"`
}

type SshDeletePayload struct {
	Ok bool `json:"ok"`
}
