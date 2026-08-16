package web

import _ "embed"

//go:embed dashboard.html
var DashboardHTML []byte

//go:embed mesh.html
var MeshHTML []byte

//go:embed chat.html
var ChatHTML []byte
