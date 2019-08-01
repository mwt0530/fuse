module github.com/jacobsa/fuse

go 1.12

require (
	git.qingstor.dev/global/common v0.0.0
	github.com/jacobsa/oglematchers v0.0.0-20150720000706-141901ea67cd
	github.com/jacobsa/oglemock v0.0.0-20150831005832-e94d794d06ff // indirect
	github.com/jacobsa/ogletest v0.0.0-20170503003838-80d50a735a11
	github.com/jacobsa/reqtrace v0.0.0-20150505043853-245c9e0234cb // indirect
	github.com/jacobsa/syncutil v0.0.0-20180201203307-228ac8e5a6c3
	github.com/jacobsa/timeutil v0.0.0-20170205232429-577e5acbbcf6
	github.com/kahing/go-xattr v1.1.1
	github.com/kylelemons/godebug v1.1.0
	golang.org/x/sys v0.0.0-20190606165138-5da285871e9c
)

replace git.qingstor.dev/global/common => git.internal.yunify.com/qingcloud-object-storage/common-go v0.0.0-20190729064521-e18acf9bf7e9
