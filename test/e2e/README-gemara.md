# Gemara (Unstructured Text) E2E Test - TODO

## Status

**NOT IMPLEMENTED** - Gemara structuring is not yet implemented in the service layer.

See `internal/catalog/service.go:257`:
```go
return nil, connect.NewError(connect.CodeUnimplemented, 
    errors.New("non-OSCAL structuring not yet implemented in service layer"))
```

## Test Fixture

- **Location**: `cmd/crosscodexd/testdata/gemara/e2e-minimal-unstructured.txt`
- **Format**: Plain text with section headers and control descriptions
- **Expected Controls**: 4 (AC-1, AC-2, AC-3, AT-1)

## When Gemara Support is Implemented

Create `test/e2e/ingest-gemara.venom.yml` following the pattern in `ingest-to-age.venom.yml`:

### Test Cases

1. **Submit Gemara catalog**
   - Base64 encode the fixture
   - POST to SubmitDocument with `catalogFormat: CATALOG_FORMAT_GEMARA`
   - Assert: jobId returned

2. **Verify format persisted**
   - Query: `SELECT format FROM catalogs WHERE name='e2e-gemara'`
   - Assert: format = "gemara"

3. **Verify controls extracted**
   - Query: `SELECT count(*) FROM controls WHERE catalog_id IN (...)`
   - Assert: count = 4

4. **Verify catalog name extraction**
   - If structurer extracts titles, verify name field populated from content
   - Otherwise, verify provided catalog_name used

## Implementation Checklist

- [ ] Implement structurer for unstructured text
- [ ] Wire structurer into ParseCatalog service layer
- [ ] Create `ingest-gemara.venom.yml` test suite
- [ ] Run test and verify all assertions pass
- [ ] Document any structurer-specific behavior or limitations
