//go:build !windows || aix

package internal

func HasPrivilegesForSymlink() bool {
	return true
}
