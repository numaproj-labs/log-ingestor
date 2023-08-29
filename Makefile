CURRENT_DIR=$(shell pwd)
DIST_DIR=${CURRENT_DIR}/dist

BINARY_NAME:=log-ingestor

# docker image publishing options
DOCKER_PUSH?=false
IMAGE_NAMESPACE?=quay.io/numaio
DOCKERFILE:=Dockerfile

VERSION?=latest
BASE_VERSION:=latest

BUILD_DATE=$(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
GIT_COMMIT=$(shell git rev-parse HEAD)
GIT_TAG=$(shell if [ -z "`git status --porcelain`" ]; then git describe --exact-match --tags HEAD 2>/dev/null; fi)
GIT_TREE_STATE=$(shell if [ -z "`git status --porcelain`" ]; then echo "clean" ; else echo "dirty"; fi)

override LDFLAGS += \
  -X ${PACKAGE}.version=${VERSION} \
  -X ${PACKAGE}.buildDate=${BUILD_DATE} \
  -X ${PACKAGE}.gitCommit=${GIT_COMMIT} \
  -X ${PACKAGE}.gitTreeState=${GIT_TREE_STATE}

ifeq (${DOCKER_PUSH},true)
ifndef IMAGE_NAMESPACE
$(error IMAGE_NAMESPACE must be set to push images (e.g. IMAGE_NAMESPACE=quay.io/numaio))
endif
endif

CURRENT_CONTEXT:=$(shell [[ "`command -v kubectl`" != '' ]] && kubectl config current-context 2> /dev/null || echo "unset")
IMAGE_IMPORT_CMD:=$(shell [[ "`command -v k3d`" != '' ]] && [[ "$(CURRENT_CONTEXT)" =~ k3d-* ]] && echo "k3d image import -c `echo $(CURRENT_CONTEXT) | cut -c 5-`")
ifndef IMAGE_IMPORT_CMD
IMAGE_IMPORT_CMD:=$(shell [[ "`command -v minikube`" != '' ]] && [[ "$(CURRENT_CONTEXT)" =~ minikube* ]] && echo "minikube image load")
endif
ifndef IMAGE_IMPORT_CMD
IMAGE_IMPORT_CMD:=$(shell [[ "`command -v kind`" != '' ]] && [[ "$(CURRENT_CONTEXT)" =~ kind-* ]] && echo "kind load docker-image")
endif

DOCKER:=$(shell command -v docker 2> /dev/null)
ifndef DOCKER
DOCKER:=$(shell command -v podman 2> /dev/null)
endif

.PHONY: test
test:
	go test $(shell go list ./... | grep -v /vendor/) -race -short -v

.PHONY: build
build: $(DIST_DIR)/$(BINARY_NAME)-linux-amd64

${DIST_DIR}/$(BINARY_NAME)-linux-amd64: GOARGS = GOOS=linux GOARCH=amd64

${DIST_DIR}/$(BINARY_NAME)-%:
	CGO_ENABLED=0 $(GOARGS) go build -v -ldflags '${LDFLAGS}' -o ${DIST_DIR}/$(BINARY_NAME) ./main.go

image: $(DIST_DIR)/$(BINARY_NAME)-linux-amd64
	$(DOCKER) build -t $(IMAGE_NAMESPACE)/$(BINARY_NAME):$(VERSION)  -f $(DOCKERFILE) .
	@if [ "$(DOCKER_PUSH)" = "true" ] ; then  docker push $(IMAGE_NAMESPACE)/$(BINARY_NAME):$(VERSION) ; fi
ifdef IMAGE_IMPORT_CMD
	$(IMAGE_IMPORT_CMD) $(IMAGE_NAMESPACE)/$(BINARY_NAME):$(VERSION)
endif

clean:
	-rm -rf ${CURRENT_DIR}/dist

.PHONY: start
start: image
	kubectl apply -f test/manifests/log-ingestor-ns.yaml
	kubectl kustomize test/manifests | sed 's@quay.io/numaio/@$(IMAGE_NAMESPACE)/@' | sed 's/:$(BASE_VERSION)/:$(VERSION)/' | kubectl -n log-ingestor apply -l app.kubernetes.io/part-of=log-ingestor --prune=false --force -f -
	kubectl -n log-ingestor wait -lapp.kubernetes.io/part-of=log-ingestor --for=condition=Ready --timeout 60s pod --all


.PHONY: manifests
manifests:
	kustomize build manifests/install > manifests/install.yaml
