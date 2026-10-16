# Remediation Plan for PR #126

**Goal:** Remediate review findings for PR #126: validate operations and send valid payload schemas for `load` in `cmd/cloudcodecheck`, guard disabled models in `catalog.DefaultID()`, and add comprehensive unit tests for disabled default fallback and `gemini-4-argon-tiered` upstream models.

**Architecture:**
- `internal/modelcatalog`: ensure `DefaultID()` checks `!model.Disabled` before selecting `gemini-4-argon-medium` or `catalog.defaultID`.
- `internal/modelcatalog`: add unit tests covering disabled `gemini-4-argon-medium` and `gemini-4-argon-tiered` upstream parsing.
- `cmd/cloudcodecheck`: unify operation dispatching so `-operation` is validated against supported operations regardless of `-endpoint`, and construct `cloudcode.LoadCodeAssistRequest` when `-operation load` is executed with `-endpoint`.

**Tech Stack:** Go 1.27, standard library.

---

### Task 1: Guard disabled models in DefaultID() and add tests

**Target files:**
- Modify: `internal/modelcatalog/catalog.go`
- Test: `internal/modelcatalog/catalog_test.go`

**Consumes / Produces:**
- Consumes: `Catalog` struct and `modelDetails` with `Disabled` field.
- Produces: `DefaultID()` returns only enabled models.

**Step 1: Write failing tests in `internal/modelcatalog/catalog_test.go`**
Add `TestGemini4ArgonDisabledDefaultFallback` and `TestGemini4ArgonTieredUpstream`:
- `TestGemini4ArgonDisabledDefaultFallback` asserts that when `gemini-4-argon-medium` is disabled in `models`, `DefaultID()` falls back to `catalog.defaultID` or the first non-disabled selectable model.
- `TestGemini4ArgonTieredUpstream` asserts that when `gemini-4-argon-tiered` is present in upstream `models`, `applyGemini4Argon` assigns `UpstreamID = "gemini-4-argon-tiered"` and appropriate `ThinkingLevel`.

**Step 2: Run test to confirm failure**
```bash
go test -v ./internal/modelcatalog -run "TestGemini4ArgonDisabledDefaultFallback"
```
Expect failure because `DefaultID()` currently returns `gemini-4-argon-medium` without checking `!model.Disabled`.

**Step 3: Minimal implementation**
In `internal/modelcatalog/catalog.go`:
```go
func (catalog *Catalog) DefaultID() string {
	if catalog == nil {
		return ""
	}
	if model, exists := catalog.byID["gemini-4-argon-medium"]; exists && !model.Disabled {
		return model.ID
	}
	if model, exists := catalog.byID[strings.ToLower(catalog.defaultID)]; exists && !model.Disabled {
		return model.ID
	}
	for _, model := range catalog.selectable {
		if !model.Disabled {
			return model.ID
		}
	}
	return ""
}
```

**Step 4: Run tests to confirm pass**
```bash
go test -v ./internal/modelcatalog -run "TestGemini4Argon.*"
```

**Step 5: Git commit**
```bash
git add internal/modelcatalog/catalog.go internal/modelcatalog/catalog_test.go
git commit -m "fix(modelcatalog): guard against disabled default model in DefaultID"
```

---

### Task 2: Fix operation validation and load request schema in cmd/cloudcodecheck

**Target files:**
- Modify: `cmd/cloudcodecheck/main.go`

**Consumes / Produces:**
- Consumes: `operation`, `project`, `endpoint` flags.
- Produces: Valid operation check and valid `LoadCodeAssistRequest` payload when targeting `-endpoint`.

**Step 1: Minimal implementation**
In `cmd/cloudcodecheck/main.go`:
Unify the `switch *operation` to validate operations and dispatch requests:
```go
	var response cloudcode.Response
	switch *operation {
	case "models":
		if *endpoint != "" {
			req := map[string]string{}
			if *project != "" {
				req["project"] = *project
			}
			response, err = client.DoJSON(ctx, []string{*endpoint}, cloudcode.PathFetchAvailableModels, req, cloudcode.RequestOptions{})
		} else {
			response, err = client.FetchAvailableModels(ctx, *project)
		}
	case "load":
		if *endpoint != "" {
			req := cloudcode.LoadCodeAssistRequest{Metadata: cloudcode.Metadata(*project), Mode: 1}
			response, err = client.DoJSON(ctx, []string{*endpoint}, cloudcode.PathLoadCodeAssist, req, cloudcode.RequestOptions{})
		} else {
			response, err = client.LoadCodeAssist(ctx, *project)
		}
	default:
		log.Fatalf("unknown operation %q", *operation)
	}
```

**Step 2: Build verification**
```bash
go build -o /dev/null ./cmd/cloudcodecheck
```

**Step 3: Run full test suite**
```bash
go test ./...
```

**Step 4: Git commit**
```bash
git add cmd/cloudcodecheck/main.go
git commit -m "fix(cloudcodecheck): validate operation and use correct payload for load with endpoint override"
```
