//go:build windows

package discover

import (
	"errors"
	"golang.org/x/sys/windows"
	"unsafe"
)

func secureCacheDirectory(path string, created bool) error {
	if !created {
		return verifyCachePrivacy(path)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return errors.New("cannot identify Windows cache owner")
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return errors.New("cannot construct private cache ACL")
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return errors.New("cannot construct private cache ACL")
	}
	if err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		return errors.New("cannot secure Windows cache directory")
	}
	return verifyCachePrivacy(path)
}

func verifyCachePrivacy(path string) error {
	invalid := func() error {
		return errors.New("Windows discover cache ACL must grant access only to the current user and SYSTEM")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return invalid()
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return invalid()
	}
	owner, _, ownerErr := descriptor.Owner()
	if ownerErr != nil || owner == nil || (owner.String() != user.User.Sid.String() && owner.String() != "S-1-5-18") {
		return invalid()
	}
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 {
		return invalid()
	}
	hasUser := false
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return invalid()
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if sid != user.User.Sid.String() && sid != "S-1-5-18" {
			return invalid()
		}
		hasUser = hasUser || sid == user.User.Sid.String()
	}
	if !hasUser {
		return invalid()
	}
	return nil
}
