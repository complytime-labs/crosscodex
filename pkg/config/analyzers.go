package config

// analyzerSections maps each production analyzer's name to its section of
// the configuration. Only classify's differs from its name.
var analyzerSections = map[string]string{
	"classify":     "analysis.classification",
	"embedding":    "analysis.embedding",
	"artifacts":    "analysis.artifacts",
	"requires":     "analysis.requires",
	"relationship": "analysis.relationship",
}

// AnalyzerSetting returns the dotted config path of setting in production
// analyzer analyzer's section, for errors that tell an operator what to
// change: AnalyzerSetting("classify", "max_tokens") is
// "analysis.classification.max_tokens". ok is false for a name that is not a
// production analyzer. It does not check that the section has setting.
func AnalyzerSetting(analyzer, setting string) (path string, ok bool) {
	section, ok := analyzerSections[analyzer]
	if !ok {
		return "", false
	}
	return section + "." + setting, true
}
