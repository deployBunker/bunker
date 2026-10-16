// Version pins. Every checksum here was fetched from the publisher's own
// endpoint at authoring time and cross-read (never hand-copied from a page):
//
//   - go:   golang.org/dl download JSON, official sha256 per file;
//   - node: nodejs.org/dist SHASUMS256.txt for the release;
//   - rust: static.rust-lang.org rustup-init.sha256, and the
//     channel-rust-<ver>.toml manifest confirming the toolchain exists.
//
// The Go pins match the ones internal/cli's gopls bootstrap already proved
// live (agenttools_packages.go); they are duplicated rather than shared so
// this package stays a leaf, and internal/cli's toolchain_check_test.go
// cross-checks the two so they can never drift apart silently.
package provisioning

const GoToolchainVersion = "go1.25.4"

const GoToolchainSHA256AMD64 = "9fa5ffeda4170de60f67f3aa0f824e426421ba724c21e133c1e35d6159ca1bec"
const GoToolchainSHA256ARM64 = "a68e86d4b72c2c2fecf7dfed667680b6c2a071221bbdb6913cf83ce3f80d9ff0"

const NodeToolchainVersion = "v20.20.2"

const NodeToolchainSHA256X64 = "df770b2a6f130ed8627c9782c988fda9669fa23898329a61a871e32f965e007d"
const NodeToolchainSHA256ARM64 = "73093db209e4e9e09dd7d15a47aeaab1b74833830df03efa5f942a1122c5fa71"

const RustToolchainVersion = "1.89.0"

const RustupInitSHA256X86_64 = "dda7234360b7f578ca8b0ddcb80145646fa61a67c1720a5abc7051b35c9fcb71"
const RustupInitSHA256AARCH64 = "15f6e4ce9f583b929c996c91562bad6d4454f3281de858b02cdfdef615fac433"
