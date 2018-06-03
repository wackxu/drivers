FROM alpine

# Copy hostpathplugin from driver directory
COPY hostpathplugin /hostpathplugin

ENTRYPOINT ["/hostpathplugin"]

