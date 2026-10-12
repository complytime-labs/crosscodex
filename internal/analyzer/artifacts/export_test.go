package artifacts

// Bridge file: exports unexported symbols for external test packages.

// ExportArtifactID exposes the deterministic Artifact node ID derivation.
var ExportArtifactID = artifactID

// ExportCleanText exposes cleanText.
var ExportCleanText = cleanText

// ExportRetryDelay exposes retryDelay.
var ExportRetryDelay = retryDelay

// ExportPromptValue exposes promptValue.
var ExportPromptValue = promptValue

// ExportMaxPromptValueBytes exposes maxPromptValueBytes.
const ExportMaxPromptValueBytes = maxPromptValueBytes

// ExportEnqueueChunk exposes enqueueChunk.
const ExportEnqueueChunk = enqueueChunk
