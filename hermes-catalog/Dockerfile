# Hermes catalog image (docs/adr/0014 section 1): the curated base profile
# plus the sync step every agent pod runs as its init container. Built by
# `make hermes-catalog` with the profile as a named build context:
#   docker buildx build --build-context profile=config/hermes/base-profile hermes-catalog
FROM python:3.13-alpine@sha256:79e7a9b9ff1cbceff819f856fb374477792a5967759d94df266de7b7b4120e6f
RUN pip install --no-cache-dir pyyaml==6.0.3 \
 && adduser -D -H -u 10001 -G nogroup catalog
COPY --chmod=0755 hermes_sync.py /usr/local/bin/hermes-sync
COPY --from=profile . /catalog
# Fails the build if catalog.yaml names a skill that does not exist.
RUN hermes-sync build /catalog && chmod -R a+rX,go-w /catalog
USER 10001
ENTRYPOINT ["hermes-sync"]
CMD ["sync"]
