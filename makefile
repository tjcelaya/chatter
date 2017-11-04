ROOT_DIR:=$(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))

build: ./dist/ca-certificates.crt ./dist/chatter-linux ./dist/chatter-image-id.txt ./vendor
	docker-compose build chatter
	docker-compose pull  consul

./dist/ca-certificates.crt:
	docker-compose build build-certs
	docker-compose run   build-certs

./dist/chatter-linux: main.go
	docker-compose build build-linux
	docker-compose run   build-linux

./dist/chatter-image-id.txt:
	docker images | grep tjcelaya/chatter | awk '{print $$3}' > ./dist/chatter-image-id.txt

./vendor:
	docker run --rm -v ${ROOT_DIR}:/go/src/github.com/tjcelaya/chatter -w /go/src/github.com/tjcelaya/chatter trifs/govendor init
	docker run --rm -v ${ROOT_DIR}:/go/src/github.com/tjcelaya/chatter -w /go/src/github.com/tjcelaya/chatter trifs/govendor fetch +missing
	docker run --rm -v ${ROOT_DIR}:/go/src/github.com/tjcelaya/chatter -w /go/src/github.com/tjcelaya/chatter trifs/govendor remove +unused

run: build
	docker-compose up   -d consul
	docker-compose logs -f
