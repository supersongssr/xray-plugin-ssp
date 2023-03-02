#!/bin/bash
cd `dirname $0`
v2ray=/usr/bin/v2ray/v2ray
config=/etc/v2ray/relay.json
eval $($(ps aux | grep "[v]2ray -config=${config}" | awk '{print "kill " $2}'))
nohup ${v2ray} -config=${config} >> v2ray.log 2>&1 &
