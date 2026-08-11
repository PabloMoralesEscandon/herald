// Package web holds the dashboard's static assets.
//
// They are embedded in the binary so Herald runs from any working directory
// with nothing to install alongside the executable.
package web

import "embed"

//go:embed index.html app.js styles.css
var Assets embed.FS
