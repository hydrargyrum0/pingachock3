//go:build windows && 386

package checks

import _ "embed"

//go:embed embedded/windows_386/xray.exe
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray.exe"
