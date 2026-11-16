// Package api holds the identifiers shared by every --json document.
package api

// SchemaV1 is the version stamp every JSON document carries. It is one string
// so a consumer can assert `.schema == "whyfail/v1"` without knowing which
// command produced the document.
const SchemaV1 = "whyfail/v1"
