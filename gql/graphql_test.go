package gql

import "testing"

func TestGenerateQueryIsDeterministic(t *testing.T) {
	type payload struct {
		ID string `json:"id"`
	}
	args := Arguments{
		"name":      Required("n"),
		"alertDays": Optional(int64(2)),
		"id":        Required("i"),
	}
	want := "mutation repoEdit($alertDays: Int, $id: String!, $name: String!) " +
		"{ repoEdit(alertDays: $alertDays, id: $id, name: $name) { id } }"
	for i := 0; i < 20; i++ {
		got, err := generateQuery(MUTATION, "repoEdit", payload{}, args)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	}
}
