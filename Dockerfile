FROM alpine

MAINTAINER Sergii Nuzhdin <ipaq.lw@gmail.com>

ADD bin/lights /usr/bin/lights

CMD /usr/bin/lights
