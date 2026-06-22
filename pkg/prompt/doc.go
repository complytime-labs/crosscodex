// Package prompt provides versioned prompt template management for LLM operations.
//
// Prompts are YAML files with structured templates, few-shot examples, and
// metadata. The package resolves prompts through a configurable layer stack
// (embedded defaults, user overrides, project overrides, CLI flags) using
// deep merge, then renders templates by substituting ${placeholder} variables
// and assembling the result into a []Message sequence suitable for LLM APIs.
//
// # Usage
//
// Create a registry from configuration:
//
//	reg, err := prompt.NewRegistry(cfg.Prompt)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
// Resolve a prompt (layered, unrendered):
//
//	spec, err := reg.Resolve(ctx, "section-detect")
//
// Render a prompt with variables:
//
//	resolved, err := reg.Render(ctx, "section-detect", map[string]string{
//	    "document_chunk": docText,
//	})
//	// resolved.Messages contains []Message ready for LLM
//	// resolved.ContentHash is SHA-256 of the rendered messages
//
// # Prompt YAML Format
//
//	name: section-detect
//	version: 1.0.0
//	metadata:
//	  domain: oscal
//	templates:
//	  system: |
//	    Analyze the section pattern in: ${document_chunk}
//	  user: |
//	    Return the regex pattern.
//	few_shot_examples:
//	  - input: "1.1 Access Control"
//	    output: '^\d+\.\d+\s+'
package prompt
