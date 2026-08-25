package main

import "fmt"

func sscan(s string, a, b *uint32) (int, error) {
	var aw, ah uint32
	n, err := fmt.Sscanf(s, "%dx%d", &aw, &ah)
	if n >= 1 {
		*a = aw
	}
	if n >= 2 {
		*b = ah
	}
	return n, err
}
