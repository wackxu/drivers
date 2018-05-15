FROM alpine

# Copy nfs from driver directory
COPY hostpathplugin /hostpathplugin

ENTRYPOINT ["/hostpathplugin"]

