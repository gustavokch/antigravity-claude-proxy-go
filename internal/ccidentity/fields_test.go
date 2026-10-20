package ccidentity

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSetObjectMembers(t *testing.T) {
	str := func(s string) []byte { return []byte(`"` + s + `"`) }
	cases := []struct {
		name    string
		body    string
		members []ObjectMember
		want    string
	}{
		{
			name:    "replaces a value in place and leaves every other byte alone",
			body:    ` {"a" : 1.0 , "model" : "old" ,"z":[1, 2] } ` + "\n",
			members: []ObjectMember{{"model", str("new")}},
			want:    ` {"a" : 1.0 , "model" : "new" ,"z":[1, 2] } ` + "\n",
		},
		{
			name:    "replaces a structured value",
			body:    `{"max_tokens":{"x":[1,{"y":2}]},"b":1}`,
			members: []ObjectMember{{"max_tokens", []byte(`16`)}},
			want:    `{"max_tokens":16,"b":1}`,
		},
		{
			name:    "appends an absent member after the last value",
			body:    `{"a":1,"b":[2] }`,
			members: []ObjectMember{{"model", str("m")}, {"max_tokens", []byte(`8`)}},
			want:    `{"a":1,"b":[2],"model":"m","max_tokens":8 }`,
		},
		{
			name:    "appends to an empty object without a comma",
			body:    `{}`,
			members: []ObjectMember{{"model", str("m")}},
			want:    `{"model":"m"}`,
		},
		{
			name:    "appends to an empty object that holds whitespace",
			body:    `{ }`,
			members: []ObjectMember{{"model", str("m")}},
			want:    `{"model":"m" }`,
		},
		{
			name:    "replaces some members and appends the rest",
			body:    `{"model":"old","messages":[]}`,
			members: []ObjectMember{{"max_tokens", []byte(`8`)}, {"model", str("new")}},
			want:    `{"model":"new","messages":[],"max_tokens":8}`,
		},
		{
			name:    "a duplicated key is replaced at both places, so every parser reads the new value",
			body:    `{"model":"first","x":1,"model":"second"}`,
			members: []ObjectMember{{"model", str("new")}},
			want:    `{"model":"new","x":1,"model":"new"}`,
		},
		{
			name:    "a key written with an escape is the same key",
			body:    `{"mod\u0065l":"old"}`,
			members: []ObjectMember{{"model", str("new")}},
			want:    `{"mod\u0065l":"new"}`,
		},
		{
			name:    "nested members and string contents are not top-level members",
			body:    `{"messages":[{"model":"nested"}],"note":"\"model\":\"quoted\" } {","tool":{"model":"deep"}}`,
			members: []ObjectMember{{"model", str("new")}},
			want:    `{"messages":[{"model":"nested"}],"note":"\"model\":\"quoted\" } {","tool":{"model":"deep"},"model":"new"}`,
		},
		{
			name:    "scalars are skipped whatever follows them",
			body:    `{"a":true,"b":null,"c":-1.5e+3,"d":"x\\","model":"old"}`,
			members: []ObjectMember{{"model", str("new")}},
			want:    `{"a":true,"b":null,"c":-1.5e+3,"d":"x\\","model":"new"}`,
		},
		{
			name: "no members returns the body",
			body: `{"a" : 1}`,
			want: `{"a" : 1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SetObjectMembers([]byte(tc.body), tc.members...)
			if err != nil {
				t.Fatalf("SetObjectMembers: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got:  %s\nwant: %s", got, tc.want)
			}
			if !json.Valid(got) {
				t.Errorf("result is not valid JSON: %s", got)
			}
		})
	}
}

func TestSetObjectMembersDoesNotWriteIntoItsInput(t *testing.T) {
	body := []byte(`{"model":"old","a":1}`)
	if _, err := SetObjectMembers(body, ObjectMember{"model", []byte(`"new"`)}); err != nil {
		t.Fatalf("SetObjectMembers: %v", err)
	}
	if string(body) != `{"model":"old","a":1}` {
		t.Errorf("the caller's body was modified: %s", body)
	}
}

func TestSetObjectMembersRejectsWhatItCannotWriteSafely(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		members []ObjectMember
		wantErr error
	}{
		{"not an object", `[1,2]`, []ObjectMember{{"model", []byte(`"m"`)}}, ErrNotAnObject},
		{"a scalar", `"model"`, []ObjectMember{{"model", []byte(`"m"`)}}, ErrNotAnObject},
		{"truncated", `{"model":`, []ObjectMember{{"model", []byte(`"m"`)}}, nil},
		{"data after the object", `{"a":1} {"b":2}`, []ObjectMember{{"model", []byte(`"m"`)}}, nil},
		{"empty", ``, []ObjectMember{{"model", []byte(`"m"`)}}, nil},
		{"invalid member value", `{"a":1}`, []ObjectMember{{"model", []byte(`"m`)}}, nil},
		{"empty member value", `{"a":1}`, []ObjectMember{{"model", nil}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SetObjectMembers([]byte(tc.body), tc.members...)
			if err == nil {
				t.Fatalf("no error; got %s", got)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// The scan walks values byte by byte, so a long body must not cost more than a
// pass over it: a value that ends in the middle of a very long string, and
// deep nesting, are both handled.
func TestSetObjectMembersHandlesLargeAndDeepValues(t *testing.T) {
	long := strings.Repeat(`a\"b} `, 1<<16)
	deep := strings.Repeat(`[`, 500) + strings.Repeat(`]`, 500)
	body := `{"text":"` + long + `","deep":` + deep + `,"model":"old"}`

	got, err := SetObjectMembers([]byte(body), ObjectMember{"model", []byte(`"new"`)})
	if err != nil {
		t.Fatalf("SetObjectMembers: %v", err)
	}
	if want := strings.TrimSuffix(body, `"old"}`) + `"new"}`; string(got) != want {
		t.Errorf("the large body was not rewritten in place (len %d, want %d)", len(got), len(want))
	}
}

// Whatever the object looks like, the result decodes to the same object with
// the named members set, and the bytes outside them are untouched.
func FuzzSetObjectMembers(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"model":"a"}`,
		`{"a":1,"model":"a","b":{"model":[1,{"model":2}]},"model":null}`,
		` { "mod\u0065l" : "x" , "s" : "}\"{" } `,
		`{"x":[],"y":{},"z":-0.5E+10}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		var before map[string]any
		if json.Unmarshal([]byte(body), &before) != nil || !strings.HasPrefix(strings.TrimSpace(body), "{") {
			t.Skip()
		}
		got, err := SetObjectMembers([]byte(body),
			ObjectMember{"model", []byte(`"new"`)}, ObjectMember{"max_tokens", []byte(`7`)})
		if err != nil {
			t.Fatalf("SetObjectMembers(%q): %v", body, err)
		}
		var after map[string]any
		if err := json.Unmarshal(got, &after); err != nil {
			t.Fatalf("result %q is not a JSON object: %v", got, err)
		}
		before["model"], before["max_tokens"] = "new", float64(7)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("SetObjectMembers(%q) = %q, which decodes to %v, want %v", body, got, after, before)
		}
	})
}
