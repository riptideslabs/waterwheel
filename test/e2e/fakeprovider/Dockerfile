# The binary is cross-compiled on the host (test/e2e/setup.sh).
FROM scratch
COPY fakeprovider /fakeprovider
USER 65532:65532
ENTRYPOINT ["/fakeprovider"]
