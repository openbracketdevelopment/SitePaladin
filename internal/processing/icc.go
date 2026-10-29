package processing

/*
#cgo pkg-config: lcms2
#include <lcms2.h>

static int bytedown_valid_icc(const void *data, unsigned int length, int bands) {
	cmsHPROFILE input = cmsOpenProfileFromMem(data, length);
	if (!input) return 0;
	cmsColorSpaceSignature space = cmsGetColorSpace(input);
	int channels = space == cmsSigRgbData ? 3 :
		space == cmsSigGrayData ? 1 : space == cmsSigCmykData ? 4 : 0;
	cmsHPROFILE output = cmsCreate_sRGBProfile();
	cmsHTRANSFORM transform = NULL;
	if (output && channels == bands)
		transform = cmsCreateTransform(input,
			cmsFormatterForColorspaceOfProfile(input, 1, FALSE),
			output, TYPE_RGB_8, INTENT_PERCEPTUAL, cmsFLAGS_NOCACHE);
	int valid = transform != NULL;
	if (transform) cmsDeleteTransform(transform);
	if (output) cmsCloseProfile(output);
	cmsCloseProfile(input);
	return valid;
}
*/
import "C"

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"unsafe"
)

// Native loaders can discard a malformed profile and then guess sRGB. Validate
// the encoded profile independently so discarded profiles cannot be accepted.
func validICC(profile []byte, bands int) bool {
	if len(profile) < 128 || len(profile) > 1<<20 ||
		int64(binary.BigEndian.Uint32(profile[:4])) != int64(len(profile)) ||
		string(profile[36:40]) != "acsp" {
		return false
	}
	return C.bytedown_valid_icc(unsafe.Pointer(&profile[0]), C.uint(len(profile)), C.int(bands)) != 0
}

func embeddedICC(data []byte, format string) ([]byte, bool, error) {
	if format == "png" {
		var profile []byte
		found := false
		for pos := 8; pos < len(data); {
			n := int(binary.BigEndian.Uint32(data[pos : pos+4]))
			if string(data[pos+4:pos+8]) == "iCCP" {
				if found {
					return nil, true, ErrInvalidImage
				}
				found = true
				body := data[pos+8 : pos+8+n]
				zero := bytes.IndexByte(body, 0)
				if zero < 1 || zero > 79 || zero+2 >= len(body) || body[zero+1] != 0 {
					return nil, true, ErrInvalidImage
				}
				z, err := zlib.NewReader(bytes.NewReader(body[zero+2:]))
				if err != nil {
					return nil, true, ErrInvalidImage
				}
				profile, err = io.ReadAll(io.LimitReader(z, (1<<20)+1))
				closeErr := z.Close()
				if err != nil || closeErr != nil || len(profile) > 1<<20 {
					return nil, true, ErrInvalidImage
				}
			}
			pos += 12 + n
		}
		return profile, found, nil
	}
	parts := map[int][]byte{}
	total := 0
	for pos := 2; pos < len(data); {
		if data[pos] != 0xff {
			return nil, false, ErrInvalidImage
		}
		for pos < len(data) && data[pos] == 0xff {
			pos++
		}
		if pos >= len(data) {
			return nil, false, ErrInvalidImage
		}
		marker := data[pos]
		pos++
		if marker == 0xda || marker == 0xd9 {
			break
		}
		if marker == 0x01 || (marker >= 0xd0 && marker <= 0xd8) {
			continue
		}
		if len(data)-pos < 2 {
			return nil, false, ErrInvalidImage
		}
		length := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		if length < 2 || length > len(data)-pos {
			return nil, false, ErrInvalidImage
		}
		body := data[pos+2 : pos+length]
		if marker == 0xe2 && bytes.HasPrefix(body, []byte("ICC_PROFILE\x00")) {
			if len(body) < 14 {
				return nil, true, ErrInvalidImage
			}
			seq, count := int(body[12]), int(body[13])
			if seq < 1 || count < 1 || seq > count || (total != 0 && total != count) || parts[seq] != nil {
				return nil, true, ErrInvalidImage
			}
			total = count
			parts[seq] = body[14:]
		}
		pos += length
	}
	if total == 0 {
		return nil, false, nil
	}
	var profile []byte
	for i := 1; i <= total; i++ {
		if parts[i] == nil || len(profile)+len(parts[i]) > 1<<20 {
			return nil, true, ErrInvalidImage
		}
		profile = append(profile, parts[i]...)
	}
	return profile, true, nil
}
