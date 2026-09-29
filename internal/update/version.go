package update

import (
	"errors"
	"strconv"
	"strings"
)

type semVersion struct {
	major, minor, patch uint64
	pre                 []string
}

func parseVersion(value string) (semVersion, error) {
	if value == "dev" || value == "" {
		return semVersion{}, errors.New("unversioned build")
	}
	text := strings.TrimPrefix(value, "v")
	core, _, _ := strings.Cut(text, "+")
	core, pre, hasPre := strings.Cut(core, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semVersion{}, errors.New("version must be major.minor.patch")
	}
	var numbers [3]uint64
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semVersion{}, errors.New("invalid version number")
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return semVersion{}, errors.New("invalid version number")
			}
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semVersion{}, err
		}
		numbers[i] = number
	}
	version := semVersion{major: numbers[0], minor: numbers[1], patch: numbers[2]}
	if hasPre {
		if pre == "" {
			return semVersion{}, errors.New("empty prerelease identifier")
		}
		version.pre = strings.Split(pre, ".")
		for _, part := range version.pre {
			if part == "" || (numeric(part) && len(part) > 1 && part[0] == '0') {
				return semVersion{}, errors.New("invalid prerelease identifier")
			}
			for _, char := range part {
				if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char == '-') {
					return semVersion{}, errors.New("invalid prerelease identifier")
				}
			}
		}
	}
	return version, nil
}

func numeric(text string) bool {
	for _, char := range text {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func compareVersion(a, b semVersion) int {
	for _, pair := range [][2]uint64{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if len(a.pre) == 0 && len(b.pre) != 0 {
		return 1
	}
	if len(a.pre) != 0 && len(b.pre) == 0 {
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		left, right := a.pre[i], b.pre[i]
		if left == right {
			continue
		}
		leftNumeric, rightNumeric := numeric(left), numeric(right)
		if leftNumeric && !rightNumeric {
			return -1
		}
		if !leftNumeric && rightNumeric {
			return 1
		}
		if leftNumeric && rightNumeric {
			if len(left) != len(right) {
				if len(left) < len(right) {
					return -1
				}
				return 1
			}
		}
		if left < right {
			return -1
		}
		return 1
	}
	if len(a.pre) < len(b.pre) {
		return -1
	}
	if len(a.pre) > len(b.pre) {
		return 1
	}
	return 0
}
