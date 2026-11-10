# contracts codegen is Pedro's task; until it lands, `generate` is a stub.
# The contracts-drift CI job activates as soon as any spec file exists here,
# so this target must exist and must be a no-op until real codegen is wired.
.PHONY: generate check
generate: check
	@echo "contracts: no codegen configured yet — nothing to generate"

check:
	@for f in schemas/*.schema.json; do \
		python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$$f" \
			|| exit 1; \
	done
	@echo "contracts: schema JSON syntax OK"
