FROM python:3.6-alpine

MAINTAINER Sergii Nuzhdin <ipaq.lw@gmail.com>

ENV INSTALL_DIR=/app

RUN apk add --update ca-certificates \
    && apk add postgresql-dev libjpeg tiff-dev zlib-dev \
               libwebp-dev gcc musl-dev linux-headers \
               libxml2-dev libxslt-dev curl\
    && rm /var/cache/apk/*

ADD requirements.txt /opt/requirements.txt
RUN pip install -r /opt/requirements.txt

RUN mkdir -p $INSTALL_DIR

WORKDIR $INSTALL_DIR

ADD . $INSTALL_DIR

CMD python app.py
