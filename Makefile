PG_REPO ?= https://github.com/postgres/postgres.git
MAJORS  := 14 15 16 17 18

.PHONY: all upstream clean $(addprefix upstream-,$(MAJORS))

all: upstream

upstream: $(addprefix upstream-,$(MAJORS))

# Per-major sparse shallow clone of the upstream postgres source. The corpus
# minor is locked to the postgres:<major> docker image's actual version, queried
# at clone time — pg_regress's expected outputs are tied to the binary that
# emits them, so corpus and image must share the same minor. Skips if the tree
# already exists; run `make clean` to refresh.
define clone_recipe
upstream-$(1): upstream/postgres-$(1)/.git
upstream/postgres-$(1)/.git:
	@if ! command -v docker >/dev/null 2>&1; then \
	    echo "docker required: clone version is derived from postgres:$(1) image" >&2; exit 1; \
	fi; \
	full=$$$$(docker run --rm postgres:$(1) postgres --version 2>/dev/null | awk '{print $$$$3}'); \
	if [ -z "$$$$full" ]; then \
	    echo "could not determine postgres:$(1) image version" >&2; exit 1; \
	fi; \
	minor=$$$${full##*.}; \
	tag="REL_$(1)_$$$${minor}"; \
	echo "cloning postgres@$$$$tag (matches postgres:$(1) image at $$$$full) into upstream/postgres-$(1)"; \
	git clone --filter=blob:none --sparse --depth=1 -b $$$$tag \
	    $(PG_REPO) upstream/postgres-$(1)
	git -C upstream/postgres-$(1) sparse-checkout set \
	    src/test/regress \
	    src/test/isolation \
	    doc/src/sgml \
	    src/include
endef
$(foreach M,$(MAJORS),$(eval $(call clone_recipe,$(M))))

clean:
	rm -rf $(addprefix upstream/postgres-,$(MAJORS))
