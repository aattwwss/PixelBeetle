package main

import "fmt"

func fmtSscan(s string, a, b *uint32) (int, error) {
	var x, y uint32
	n, err := fmt.Sscanf(s, "%d:%d", &x, &y)
	if n >= 1 {
		*a = x
	}
	if n >= 2 {
		*b = y
	}
	return n, err
}
