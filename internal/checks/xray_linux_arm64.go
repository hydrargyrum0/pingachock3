//go:build linux && arm64

package checks

import _ "embed"

//go:embed embedded/linux_arm64/xray
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray"
