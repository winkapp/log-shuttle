FROM heroku/log-shuttle:0.16.0

RUN apk update

RUN apk add wget sudo bash socat

ADD ./heroku_kinesis.sh /root/

EXPOSE 514

ENTRYPOINT ["/bin/bash", "-c"]
CMD ["/root/heroku_kinesis.sh"]
