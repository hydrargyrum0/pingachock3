//go:build windows && amd64

package checks

import _ "embed"

//go:embed embedded/windows_amd64/xray.exe
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray.exe"
