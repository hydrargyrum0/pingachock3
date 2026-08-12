//go:build darwin && amd64

package checks

import _ "embed"

//go:embed embedded/darwin_amd64/xray
var embeddedXrayBinary []byte

const embeddedXrayFilename = "xray"
