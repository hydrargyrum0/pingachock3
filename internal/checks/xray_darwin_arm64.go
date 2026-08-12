//go:build darwin && arm64

package checks

import _ "embed"

//go:embed embedded/darwin_arm64/xray
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray"
