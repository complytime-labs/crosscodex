// Package crosswalk scores crosswalk results against the references a catalog
// cites (#260). It is a test tool: it reads catalog and result files only and
// needs no running daemon. Identifier handling is driven by the target catalog
// (see TargetIndex), never by knowledge of any framework's ID syntax.
package crosswalk
