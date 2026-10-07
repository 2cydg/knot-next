//go:build windows

package logger

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Apply a protected DACL granting only the current user access. Unix mode bits
// do not describe Windows ACLs. File descendants inherit the same policy.
func secureDirectory(path string) error { return secureWindowsPath(path) }
func secureFile(file *os.File) error    { return secureWindowsPath(file.Name()) }
func secureWindowsPath(path string) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;%s)", user.User.Sid.String()))
	if err != nil {
		return err
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
