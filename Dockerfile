FROM scratch

ADD ./dist/ca-certificates.crt  /etc/ssl/certs/ca-certificates.crt
ADD ./dist/chatter-linux        /chatter

ENTRYPOINT ["/chatter"]