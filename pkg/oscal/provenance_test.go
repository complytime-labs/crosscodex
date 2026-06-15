package oscal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"
)

func TestProvenance_NewProvenance_CreatesReader(t *testing.T) {
	content := "test content for hashing"
	reader := strings.NewReader(content)

	prov, teeReader, err := NewProvenance(reader)
	if err != nil {
		t.Fatalf("NewProvenance failed: %v", err)
	}

	if prov == nil {
		t.Fatal("Expected non-nil Provenance")
	}

	if teeReader == nil {
		t.Fatal("Expected non-nil tee reader")
	}

	// Verify timestamp is recent
	if time.Since(prov.RetrievalTimestamp) > time.Second {
		t.Error("RetrievalTimestamp should be recent")
	}

	// ContentHash should be empty until reader is consumed
	if prov.ContentHash != "" {
		t.Error("ContentHash should be empty before reading")
	}
	if prov.ContentSize != 0 {
		t.Error("ContentSize should be zero before reading")
	}
}

func TestProvenance_TeeReader_ComputesHash(t *testing.T) {
	content := "test content for hashing"
	reader := strings.NewReader(content)

	prov, teeReader, err := NewProvenance(reader)
	if err != nil {
		t.Fatalf("NewProvenance failed: %v", err)
	}

	// Read all content
	data, err := io.ReadAll(teeReader)
	if err != nil {
		t.Fatalf("Failed to read: %v", err)
	}

	// Verify content is unchanged
	if string(data) != content {
		t.Errorf("Expected content '%s', got '%s'", content, string(data))
	}

	// Compute expected hash
	hash := sha256.Sum256([]byte(content))
	expectedHash := hex.EncodeToString(hash[:])

	// Verify hash was computed
	if prov.ContentHash != expectedHash {
		t.Errorf("Expected hash '%s', got '%s'", expectedHash, prov.ContentHash)
	}

	// Verify size was recorded
	expectedSize := int64(len(content))
	if prov.ContentSize != expectedSize {
		t.Errorf("Expected size %d, got %d", expectedSize, prov.ContentSize)
	}
}

func TestProvenance_TeeReader_HandlesEmptyContent(t *testing.T) {
	reader := strings.NewReader("")

	prov, teeReader, err := NewProvenance(reader)
	if err != nil {
		t.Fatalf("NewProvenance failed: %v", err)
	}

	// Read all content
	data, err := io.ReadAll(teeReader)
	if err != nil {
		t.Fatalf("Failed to read: %v", err)
	}

	if len(data) != 0 {
		t.Errorf("Expected empty data, got %d bytes", len(data))
	}

	// Hash of empty content
	hash := sha256.Sum256([]byte(""))
	expectedHash := hex.EncodeToString(hash[:])

	if prov.ContentHash != expectedHash {
		t.Errorf("Expected hash '%s', got '%s'", expectedHash, prov.ContentHash)
	}

	if prov.ContentSize != 0 {
		t.Errorf("Expected size 0, got %d", prov.ContentSize)
	}
}

func TestProvenance_TeeReader_HandlesLargeContent(t *testing.T) {
	// Create 1MB of content
	content := bytes.Repeat([]byte("a"), 1024*1024)
	reader := bytes.NewReader(content)

	prov, teeReader, err := NewProvenance(reader)
	if err != nil {
		t.Fatalf("NewProvenance failed: %v", err)
	}

	// Read all content
	data, err := io.ReadAll(teeReader)
	if err != nil {
		t.Fatalf("Failed to read: %v", err)
	}

	if len(data) != len(content) {
		t.Errorf("Expected %d bytes, got %d bytes", len(content), len(data))
	}

	// Compute expected hash
	hash := sha256.Sum256(content)
	expectedHash := hex.EncodeToString(hash[:])

	if prov.ContentHash != expectedHash {
		t.Errorf("Hash mismatch for large content")
	}

	if prov.ContentSize != int64(len(content)) {
		t.Errorf("Expected size %d, got %d", len(content), prov.ContentSize)
	}
}

func TestProvenance_SetExtractor(t *testing.T) {
	prov := &Provenance{}

	prov.SetExtractor("TestExtractor", "v1.2.3")

	if prov.ExtractorName != "TestExtractor" {
		t.Errorf("Expected ExtractorName 'TestExtractor', got '%s'", prov.ExtractorName)
	}
	if prov.ExtractorVersion != "v1.2.3" {
		t.Errorf("Expected ExtractorVersion 'v1.2.3', got '%s'", prov.ExtractorVersion)
	}
}

func TestProvenance_SetOutputHash(t *testing.T) {
	prov := &Provenance{}
	data := []byte("output data")

	prov.SetOutputHash(data)

	// Compute expected hash
	hash := sha256.Sum256(data)
	expectedHash := hex.EncodeToString(hash[:])

	if prov.OutputHash != expectedHash {
		t.Errorf("Expected OutputHash '%s', got '%s'", expectedHash, prov.OutputHash)
	}
}

func TestProvenance_SetOutputHash_EmptyData(t *testing.T) {
	prov := &Provenance{}
	data := []byte("")

	prov.SetOutputHash(data)

	// Hash of empty data
	hash := sha256.Sum256(data)
	expectedHash := hex.EncodeToString(hash[:])

	if prov.OutputHash != expectedHash {
		t.Errorf("Expected OutputHash '%s', got '%s'", expectedHash, prov.OutputHash)
	}
}

func TestProvenance_FullWorkflow(t *testing.T) {
	// Simulate full provenance workflow
	inputContent := "source document content"
	outputContent := "extracted OSCAL JSON"

	// Create provenance with tee reader
	reader := strings.NewReader(inputContent)
	prov, teeReader, err := NewProvenance(reader)
	if err != nil {
		t.Fatalf("NewProvenance failed: %v", err)
	}

	// Set source metadata
	prov.SourceURI = "file:///path/to/source.pdf"
	prov.Format = "pdf"

	// Consume the reader (simulating extraction)
	_, err = io.ReadAll(teeReader)
	if err != nil {
		t.Fatalf("Failed to read: %v", err)
	}

	// Set extractor info
	prov.SetExtractor("PDFExtractor", "v2.1.0")

	// Set output hash
	prov.SetOutputHash([]byte(outputContent))

	// Verify all fields are populated
	if prov.SourceURI != "file:///path/to/source.pdf" {
		t.Errorf("Expected SourceURI 'file:///path/to/source.pdf', got '%s'", prov.SourceURI)
	}
	if prov.Format != "pdf" {
		t.Errorf("Expected Format 'pdf', got '%s'", prov.Format)
	}
	if prov.ContentHash == "" {
		t.Error("Expected ContentHash to be set")
	}
	if prov.ContentSize != int64(len(inputContent)) {
		t.Errorf("Expected ContentSize %d, got %d", len(inputContent), prov.ContentSize)
	}
	if prov.OutputHash == "" {
		t.Error("Expected OutputHash to be set")
	}
	if prov.ExtractorName != "PDFExtractor" {
		t.Errorf("Expected ExtractorName 'PDFExtractor', got '%s'", prov.ExtractorName)
	}
	if prov.ExtractorVersion != "v2.1.0" {
		t.Errorf("Expected ExtractorVersion 'v2.1.0', got '%s'", prov.ExtractorVersion)
	}
	if time.Since(prov.RetrievalTimestamp) > time.Second {
		t.Error("RetrievalTimestamp should be recent")
	}
}

func TestProvenance_String(t *testing.T) {
	prov := &Provenance{
		SourceURI:          "file:///test.pdf",
		RetrievalTimestamp: time.Now(),
		ContentHash:        "abc123",
		ContentSize:        1024,
		Format:             "pdf",
		OutputHash:         "def456",
		ExtractorName:      "TestExtractor",
		ExtractorVersion:   "v1.0.0",
	}

	str := prov.String()

	// Verify string contains key fields
	if !strings.Contains(str, "file:///test.pdf") {
		t.Error("String should contain SourceURI")
	}
	if !strings.Contains(str, "abc123") {
		t.Error("String should contain ContentHash")
	}
	if !strings.Contains(str, "1024") {
		t.Error("String should contain ContentSize")
	}
	if !strings.Contains(str, "pdf") {
		t.Error("String should contain Format")
	}
	if !strings.Contains(str, "def456") {
		t.Error("String should contain OutputHash")
	}
	if !strings.Contains(str, "TestExtractor") {
		t.Error("String should contain ExtractorName")
	}
	if !strings.Contains(str, "v1.0.0") {
		t.Error("String should contain ExtractorVersion")
	}
}

func TestProvenance_TeeReader_PartialReads(t *testing.T) {
	content := "test content for partial reading"
	reader := strings.NewReader(content)

	prov, teeReader, err := NewProvenance(reader)
	if err != nil {
		t.Fatalf("NewProvenance failed: %v", err)
	}

	// Read in chunks
	buf := make([]byte, 5)
	var total int
	for {
		n, err := teeReader.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read failed: %v", err)
		}
	}

	// Verify total bytes read
	if total != len(content) {
		t.Errorf("Expected to read %d bytes, got %d", len(content), total)
	}

	// Verify hash is correct
	hash := sha256.Sum256([]byte(content))
	expectedHash := hex.EncodeToString(hash[:])

	if prov.ContentHash != expectedHash {
		t.Errorf("Expected hash '%s', got '%s'", expectedHash, prov.ContentHash)
	}

	if prov.ContentSize != int64(len(content)) {
		t.Errorf("Expected size %d, got %d", len(content), prov.ContentSize)
	}
}
