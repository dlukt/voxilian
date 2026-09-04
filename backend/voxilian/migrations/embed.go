package migrations

import "embed"

// FS embeds all goose migration sources into the binary so the
// one-shot migrate container and local runs never depend on a
// migrations directory beside the executable.
//
//go:embed *.sql
var FS embed.FS
