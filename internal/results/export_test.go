package results

// DecodeContributions exposes decodeContributions to the external test package.
var DecodeContributions = decodeContributions

// AttachCleaning exposes attachCleaning, so its error paths can be driven
// with a scripted transaction instead of a database.
var AttachCleaning = attachCleaning
