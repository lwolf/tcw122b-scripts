FROM alpine

MAINTAINER Sergii Nuzhdin <ipaq.lw@gmail.com>

RUN apk update \
    && apk add ca-certificates tzdata \
    && rm -rf /var/cache/apk/*

ADD bin/lights /usr/bin/lights

CMD /usr/bin/lights
