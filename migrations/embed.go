// Package migrations holds the single embedded SQL migration source.
package migrations

import _ "embed"

//go:embed 0001_core.sql
var Core string

//go:embed 0002_content_files.sql
var Content string

//go:embed 0003_file_cleanup.sql
var FileCleanup string

//go:embed 0004_runtime.sql
var Runtime string

//go:embed 0005_memory_governance.sql
var MemoryGovernance string
