package attestation

import "time"

// Step represents a pipeline step.
type Step struct {
	Name              string   // Step name
	ExpectedMaterials []string // Expected input artifacts
	ExpectedProducts  []string // Expected output artifacts
	Command           []string // Command executed
	Threshold         int      // Signature threshold
}

// Inspection defines a post-execution verification.
type Inspection struct {
	Name   string   // Inspection name
	Run    []string // Inspection command
	Passes []string // Success criteria
}

// Artifact represents a file or artifact with its content digest.
type Artifact struct {
	URI    string // Artifact URI or path
	Digest string // SHA256 hex digest
}

// LayoutOptions configures a supply chain layout.
type LayoutOptions struct {
	Steps       []Step
	Inspections []Inspection
	ExpiresIn   time.Duration // Layout validity period
}

// SignedLayout is a signed in-toto layout envelope.
type SignedLayout struct {
	Raw     []byte    // Serialized signed envelope (JSON)
	Expires time.Time // Computed from ExpiresIn at creation
}

// SignedLink is a signed in-toto link envelope with trace correlation.
type SignedLink struct {
	Raw       []byte     // Serialized signed envelope (JSON)
	Step      string     // Step name
	TraceID   string     // OTel trace ID embedded in ByProducts["trace_id"]
	Materials []Artifact // Input artifacts
	Products  []Artifact // Output artifacts
}

// VerifiedLink is the result of verifying a signed link envelope.
type VerifiedLink struct {
	Step       string         // Step name
	Materials  []Artifact     // Input artifacts
	Products   []Artifact     // Output artifacts
	ByProducts map[string]any // Additional metadata including trace_id
}
