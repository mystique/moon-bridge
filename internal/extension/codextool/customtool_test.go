package codextool

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRebuildApplyPatchGrammarUpdateFileIncludesValidPatchMarkers(t *testing.T) {
	input := json.RawMessage(`{
		"path":"internal/example.go",
		"move_to":"internal/example_v2.go",
		"hunks":[
			{
				"context":"func demo()",
				"lines":[
					{"op":"context","text":"func demo() {"},
					{"op":"remove","text":"\told()"},
					{"op":"add","text":"\tnew()"},
					{"op":"context","text":"}"}
				]
			}
		]
	}`)

	got := RebuildApplyPatchGrammar("apply_patch_update_file", input)

	for _, want := range []string{
		"*** Begin Patch\n",
		"*** Update File: internal/example.go\n",
		"*** Move to: internal/example_v2.go\n",
		"@@ func demo()\n",
		" func demo() {\n",
		"-\told()\n",
		"+\tnew()\n",
		" }\n",
		"*** End Patch\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rebuilt patch missing %q:\n%s", want, got)
		}
	}
}

func TestRebuildApplyPatchGrammarBatchPreservesAllOperations(t *testing.T) {
	input := json.RawMessage(`{
		"operations":[
			{"type":"add_file","path":"new.txt","content":"hello\nworld"},
			{"type":"delete_file","path":"old.txt"},
			{
				"type":"update_file",
				"path":"edit.txt",
				"hunks":[
					{
						"context":"header",
						"lines":[
							{"op":"context","text":"same"},
							{"op":"add","text":"added"}
						]
					}
				]
			}
		]
	}`)

	got := RebuildApplyPatchGrammar("apply_patch_batch", input)

	if strings.Count(got, "*** Begin Patch\n") != 3 {
		t.Fatalf("expected 3 begin markers, got:\n%s", got)
	}
	for _, want := range []string{
		"*** Add File: new.txt\n+hello\n+world\n*** End Patch\n",
		"*** Delete File: old.txt\n*** End Patch\n",
		"*** Update File: edit.txt\n@@ header\n same\n+added\n*** End Patch\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rebuilt batch missing %q:\n%s", want, got)
		}
	}
}

func TestRebuildGrammarUsesRawInputForGenericCustomTools(t *testing.T) {
	got := RebuildGrammar("custom_tool", json.RawMessage(`{"input":"plain freeform body"}`))
	if got != "plain freeform body" {
		t.Fatalf("RebuildGrammar() = %q, want raw input", got)
	}
}

func TestEncodeNamespacedHistoryCall(t *testing.T) {
	args := json.RawMessage(`{"path":"/etc/hosts"}`)

	tests := []struct {
		name      string
		strategy  NamespaceStrategy
		wantName  string
		wantInput string
	}{
		{"flat", Flat, "mcp__fs_read", `{"path":"/etc/hosts"}`},
		{"nested_oneof", NestedOneOf, "mcp__fs", `{"action":"read","path":"/etc/hosts"}`},
		{"nested_anyof", NestedAnyOf, "mcp__fs", `{"action":"read","params":{"path":"/etc/hosts"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotInput := EncodeNamespacedHistoryCall("mcp__fs", "read", args, tc.strategy)
			if gotName != tc.wantName {
				t.Fatalf("name = %q, want %q", gotName, tc.wantName)
			}
			if string(gotInput) != tc.wantInput {
				t.Fatalf("input = %s, want %s", string(gotInput), tc.wantInput)
			}
		})
	}
}

func TestEncodeNamespacedHistoryCallEmptyNamespacePassesThrough(t *testing.T) {
	args := json.RawMessage(`{"x":1}`)
	gotName, gotInput := EncodeNamespacedHistoryCall("", "plain", args, Flat)
	if gotName != "plain" || string(gotInput) != `{"x":1}` {
		t.Fatalf("got name=%q input=%s, want passthrough", gotName, string(gotInput))
	}
}

func TestEncodeNamespacedHistoryCallNormalizesEmptyInput(t *testing.T) {
	// Flat with empty input → "{}"; NestedAnyOf wraps params as "{}".
	gotName, gotInput := EncodeNamespacedHistoryCall("mcp__fs", "read", json.RawMessage(`{}`), Flat)
	if gotName != "mcp__fs_read" || string(gotInput) != `{}` {
		t.Fatalf("flat empty: name=%q input=%s", gotName, string(gotInput))
	}
	gotName, gotInput = EncodeNamespacedHistoryCall("mcp__fs", "read", json.RawMessage(``), NestedAnyOf)
	if gotName != "mcp__fs" || string(gotInput) != `{"action":"read","params":{}}` {
		t.Fatalf("anyof empty: name=%q input=%s", gotName, string(gotInput))
	}
}
