<<<<<<< HEAD
FROM alpine
=======
FROM centos:7.4.1708
>>>>>>> 92fed8d... vendor fix

# Copy nfs from driver directory
COPY hostpathplugin /hostpathplugin

ENTRYPOINT ["/hostpathplugin"]

