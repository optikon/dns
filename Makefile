# Makefile for Optikon DNS.

IMAGE ?= optikon/coredns
TAG ?= 2.1.1

# Build the custom CoreDNS Docker image.
.PHONY: all
all:
	docker build -t $(IMAGE):$(TAG) .
	docker rmi -f $$(docker images -q -f dangling=true)

# Removes all object and executable files.
.PHONY: clean
clean:
	docker image rm -f $(IMAGE):$(TAG)

# Removes and rebuilds everything.
.PHONY: fresh
fresh: clean all
