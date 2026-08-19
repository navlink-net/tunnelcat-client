// The Tunnel Cat Project
// Copyright (C) NavLink, 2026
// Лицензировано под лицензией Apache 2.0

//go:build windows

package windows

import (
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"syscall"
	"unsafe"

	gozxing "github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

var comdlg32 = syscall.NewLazyDLL("comdlg32.dll")

// openQRImageFileDialog shows a Windows open-file dialog filtered for image files.
// Returns the selected path and true; ("", false) on cancel or error.
func openQRImageFileDialog(hwndOwner uintptr) (string, bool) {
	var buf [4096]uint16
	// Double-null-terminated filter string.
	filter, _ := syscall.UTF16PtrFromString("Image Files\x00*.png;*.jpg;*.jpeg;*.bmp\x00All Files\x00*.*\x00\x00")
	title, _ := syscall.UTF16PtrFromString("Select QR Code Image")
	ofn := qrOpenFileName{
		lStructSize: uint32(unsafe.Sizeof(qrOpenFileName{})),
		hwndOwner:   hwndOwner,
		lpstrFilter: filter,
		lpstrFile:   &buf[0],
		nMaxFile:    uint32(len(buf)),
		lpstrTitle:  title,
		flags:       0x00001000, // OFN_FILEMUSTEXIST
	}
	r, _, _ := comdlg32.NewProc("GetOpenFileNameW").Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return "", false
	}
	return syscall.UTF16ToString(buf[:]), true
}

// qrOpenFileName mirrors OPENFILENAMEW for GetOpenFileNameW.
type qrOpenFileName struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	_pad0             uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	_pad1             uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

// decodeQRFromImageFile opens the image at path and returns the text of the first
// QR code found, or an error if none is detected.
func decodeQRFromImageFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", fmt.Errorf("bitmap: %w", err)
	}
	result, err := qrcode.NewQRCodeReader().Decode(bmp, nil)
	if err != nil {
		return "", fmt.Errorf("no QR code found in image")
	}
	return result.GetText(), nil
}
