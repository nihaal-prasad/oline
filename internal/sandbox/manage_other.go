//go:build !windows

package sandbox

import "context"

// Non-Windows stubs: these management helpers are WSL-specific. On Linux use the
// QEMU backend (its equivalents land with the VM lifecycle).

func ListWSLEngagements(context.Context, string) ([]string, error) { return nil, errNoWSL }
func ResolveWSLEngagementPath(context.Context, string, string) (string, error) {
	return "", errNoWSL
}
func CopyWSLDirToWorkspace(context.Context, string, string, string) error { return errNoWSL }
func ReadWSLFile(context.Context, string, string) (string, error)         { return "", errNoWSL }
func CopyFileToWSL(context.Context, string, string, string) error         { return errNoWSL }
func ImportWSLEngagement(context.Context, string, string, string) error {
	return errNoWSL
}
func StartWSLVPN(context.Context, string, string) (string, error) { return "", errNoWSL }
func VPNStatus(context.Context, string) (bool, string)            { return false, "" }
func RunAsRoot(context.Context, string, string) (string, error)   { return "", errNoWSL }
func BackupWSLEngagement(context.Context, string, string, string) error {
	return errNoWSL
}
