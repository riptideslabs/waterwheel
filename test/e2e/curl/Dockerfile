# The demo's curl image with the e2e CA in its system bundle, like an app image
# with a private CA baked in. Step 2 then gets a 401 rather than a TLS error,
# and the CA must survive Waterwheel's bundle mount in step 4 (waterwheel-extra-roots).
FROM curlimages/curl:8.16.0
USER root
COPY ca.crt /tmp/e2e-ca.crt
# The image points curl at its own bundle (CURL_CA_BUNDLE=/cacert.pem), so add
# the CA there too.
RUN cat /tmp/e2e-ca.crt >> /etc/ssl/certs/ca-certificates.crt && \
    cat /tmp/e2e-ca.crt >> "$CURL_CA_BUNDLE" && rm /tmp/e2e-ca.crt
USER curl_user
