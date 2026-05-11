package attestation

// Layout defines the expected supply chain workflow.
type Layout struct {
	Steps   []Step            // Pipeline steps
	Inspect []Inspection      // Inspections to perform
	Keys    map[string][]byte // Public keys for verification
	Expires int64             // Expiration timestamp
}

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

// Link represents the execution record of a step.
type Link struct {
	Name       string         // Step name
	Materials  []Artifact     // Input artifacts
	Products   []Artifact     // Output artifacts
	Command    []string       // Executed command
	ByProducts map[string]any // Additional metadata
}

// Artifact represents a file or artifact.
type Artifact struct {
	URI    string // Artifact URI or path
	Digest string // SHA256 digest
}

// Signature represents a cryptographic signature.
type Signature struct {
	KeyID     string // Key identifier
	Signature []byte // Signature bytes
}
