//go:build linux && amd64

package checks

import _ "embed"

//go:embed embedded/linux_amd64/xray
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray"
