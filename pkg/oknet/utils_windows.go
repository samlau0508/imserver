//go:build windows
// +build windows

package oknet

func GetMaxOpenFiles() int {
	return 1024 * 1024 * 2
}
