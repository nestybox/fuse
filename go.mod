module github.com/nestybox/sysbox-fs/bazil

go 1.13

require (
	bazil.org/fuse v0.0.0-00010101000000-000000000000
	golang.org/x/net v0.7.0
	golang.org/x/sys v0.0.0-20220412211240-33da011f77ad
)

replace bazil.org/fuse => ./
