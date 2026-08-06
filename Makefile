.PHONY: build check fmt fmt-check matrix-check scenario-boundary-check test test-race vet

GO_DIRS := cmd internal skills
MATRIX_DOC := AGENTS.md
ENFORCED_TESTS := quality/enforced-tests.txt

build:
	go build -o courtd ./cmd/courtd

fmt:
	gofmt -w $(GO_DIRS)

fmt-check:
	@files="$$(gofmt -l $(GO_DIRS))"; \
	if [ -n "$$files" ]; then \
		echo "Go files need formatting:"; \
		echo "$$files"; \
		exit 1; \
	fi

vet:
	go vet ./...

scenario-boundary-check:
	@status=0; \
	grep -En 'encoding/json|net/http|httptest|internal/(api|mcp)|golden|snapshot' internal/core/*_test.go || status=$$?; \
	if [ "$$status" -eq 0 ]; then \
		echo "Core scenario tests must not depend on wire formats or transport packages."; \
		exit 1; \
	elif [ "$$status" -ne 1 ]; then \
		echo "Could not inspect core scenario tests."; \
		exit "$$status"; \
	fi

matrix-check:
	@documented="$$(awk -F'|' '$$4 ~ /enforced by/ { print $$3 }' "$(MATRIX_DOC)" | grep -Eo 'Test[A-Za-z0-9_]+' | sort -u)"; \
	manifested="$$(awk 'NF == 2 && $$1 !~ /^#/ { print $$2 }' "$(ENFORCED_TESTS)" | sort -u)"; \
	if [ "$$documented" != "$$manifested" ]; then \
		echo "Enforced test names differ between $(MATRIX_DOC) and $(ENFORCED_TESTS)."; \
		echo "Documented:"; echo "$$documented"; \
		echo "Manifested:"; echo "$$manifested"; \
		exit 1; \
	fi; \
	while read -r package test_name; do \
		case "$$package" in ''|'#'*) continue ;; esac; \
		listed="$$(go test "$$package" -list "^$${test_name}$$")"; \
		if ! echo "$$listed" | grep -qx "$$test_name"; then \
			echo "Go test runner did not discover $$test_name in $$package"; \
			exit 1; \
		fi; \
	done < "$(ENFORCED_TESTS)"

test:
	go test ./...

test-race:
	go test -race ./...

check: fmt-check scenario-boundary-check matrix-check vet test
