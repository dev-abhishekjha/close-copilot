package web

import "mime"

// Register the static asset types the host's mime table may lack, so
// /static/fonts/*.woff2 and /static/icons.svg are served with the right type.
func init() {
	_ = mime.AddExtensionType(".woff2", "font/woff2")
	_ = mime.AddExtensionType(".svg", "image/svg+xml")
}
