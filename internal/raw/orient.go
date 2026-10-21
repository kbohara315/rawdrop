package raw

// jpegTransform returns the jpegtran switches that turn a JPEG tagged with
// EXIF orientation o into its upright orientation. The mapping matches PIL's
// ImageOps.exif_transpose: mirrored orientations use transpose/transverse
// because jpegtran refuses combined flip and rotate switches.
//
// A nil result means no transform is needed (orientation 1) or understood.
func jpegTransform(o int) []string {
	switch o {
	case 2:
		return []string{"-flip", "horizontal"}
	case 3:
		return []string{"-rotate", "180"}
	case 4:
		return []string{"-flip", "vertical"}
	case 5:
		return []string{"-transpose"}
	case 6:
		return []string{"-rotate", "90"}
	case 7:
		return []string{"-transverse"}
	case 8:
		return []string{"-rotate", "270"}
	default:
		return nil
	}
}

// needsRotation reports whether CorrectOrientation asks for a physical
// rotation of a photo with this EXIF orientation.
func needsRotation(orientation int, correctOrientation bool) bool {
	return correctOrientation && jpegTransform(orientation) != nil
}

// finalOrientation returns the EXIF orientation the exported JPEG must carry:
// 1 after a successful physical rotation, otherwise the source orientation.
func finalOrientation(orientation int, rotated bool) int {
	if rotated {
		return 1
	}
	if orientation >= 1 && orientation <= 8 {
		return orientation
	}
	return 1
}

// swapsDims reports whether an orientation 5-8 transform exchanges width and
// height.
func swapsDims(orientation int) bool {
	return orientation >= 5 && orientation <= 8
}

// isFullSize reports whether an embedded JPEG with stored dimensions (jw, jh)
// is at least 90% of the camera's usable displayed dimensions. Embedded
// JPEGs are normally stored in sensor orientation, like the EXIF dimensions,
// so the comparison is direct; orientations 5-8 additionally accept a
// pre-rotated JPEG whose dimensions are swapped.
func isFullSize(jw, jh int, info Info) bool {
	if info.Width <= 0 || info.Height <= 0 || jw <= 0 || jh <= 0 {
		return false
	}
	if dimsCover(jw, jh, info.Width, info.Height) {
		return true
	}
	return swapsDims(info.Orientation) && dimsCover(jw, jh, info.Height, info.Width)
}

// dimsCover reports whether (w, h) reaches at least 90% of (needW, needH).
// Integer arithmetic avoids rounding surprises at the boundary.
func dimsCover(w, h, needW, needH int) bool {
	return w*10 >= needW*9 && h*10 >= needH*9
}
