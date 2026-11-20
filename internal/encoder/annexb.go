package encoder

// HasIDR reports whether an Annex-B access unit holds an IDR slice.
func HasIDR(au []byte) bool {
	for i := 0; i+3 < len(au); i++ {
		if au[i] != 0 || au[i+1] != 0 {
			continue
		}
		start := -1
		switch {
		case au[i+2] == 1:
			start = i + 3
		case au[i+2] == 0 && i+4 < len(au) && au[i+3] == 1:
			start = i + 4
		}
		if start >= 0 && start < len(au) {
			if au[start]&0x1f == 5 {
				return true
			}
			i = start - 1
		}
	}
	return false
}
