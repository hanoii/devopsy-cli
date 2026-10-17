package images

// cksum is POSIX cksum's CRC of data, as `printf '%s' ref | cksum`
// prints it: the suffix of a bookmark whose repository a release names
// twice, as v0.38's scripts named them too.
func cksum(data []byte) uint32 {
	var crc uint32
	add := func(b byte) {
		crc = crc<<8 ^ cksumTable[byte(crc>>24)^b]
	}
	for _, b := range data {
		add(b)
	}
	for n := len(data); n > 0; n >>= 8 {
		add(byte(n))
	}
	return ^crc
}

var cksumTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()
