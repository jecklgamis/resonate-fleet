# Helm charts for the two server components: worker/ and controller/.
# Targets take the component as a suffix, e.g. `make install-worker`; the
# unsuffixed ones act on both.
COMPONENTS := worker controller

# chart name and version for a component, e.g. resonate-worker-0.1.0.tgz
version = $(shell yq .version $(1)/Chart.yaml)
tgz = resonate-$(1)-$(call version,$(1)).tgz

default:
	@cat ./Makefile

lint: $(addprefix lint-,$(COMPONENTS))
package: $(addprefix package-,$(COMPONENTS))
clean:
	@rm -f resonate-*.tgz dry-run-*.txt
all: package

$(addprefix lint-,$(COMPONENTS)): lint-%:
	helm lint ./$*
$(addprefix package-,$(COMPONENTS)): package-%:
	@rm -f resonate-$*-*.tgz
	helm package ./$*
$(addprefix dry-run-,$(COMPONENTS)): dry-run-%:
	@helm install --dry-run --debug resonate-$* ./$* | tee dry-run-$*.txt
$(addprefix install-,$(COMPONENTS)): install-%: package-%
	@echo $(call version,$*)
	@helm install resonate-$* $(call tgz,$*)
$(addprefix upgrade-,$(COMPONENTS)): upgrade-%: package-%
	@helm upgrade resonate-$* $(call tgz,$*)
$(addprefix uninstall-,$(COMPONENTS)): uninstall-%:
	@helm uninstall resonate-$*
$(addprefix rollback-,$(COMPONENTS)): rollback-%:
	@helm rollback resonate-$*

.PHONY: default lint package clean all \
	$(addprefix lint-,$(COMPONENTS)) $(addprefix package-,$(COMPONENTS)) \
	$(addprefix dry-run-,$(COMPONENTS)) $(addprefix install-,$(COMPONENTS)) \
	$(addprefix upgrade-,$(COMPONENTS)) $(addprefix uninstall-,$(COMPONENTS)) \
	$(addprefix rollback-,$(COMPONENTS))
