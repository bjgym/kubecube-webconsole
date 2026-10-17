#!/bin/bash

declare -a headers
i=0
while getopts "u:c:h:" opt; do
  case $opt in
    u)
      USERNAME=$OPTARG
      ;;
    c)
      CLUSTER=$OPTARG
      ;;
    h)
      headers[$i]="--header=$OPTARG"
      let i+=1
      ;;
    \?)
      echo "Invalid option: -$OPTARG"
      ;;
  esac
done
DIR=$HOME/$USERNAME-$CLUSTER
TMP_CONFIG_NAME=$(cat /proc/sys/kernel/random/uuid)

USR_BIN_ARR=(vi vim xargs)
BIN_ARR=(bash ls rm grep cat less mkdir echo)

if [ ! -d $DIR ]; then
    ### 无法使用ln后chroot，只能拷贝
    mkdir -p $DIR
    mkdir -p $DIR/root/.kube
    mkdir -p $DIR/tmp
    # Derived, not hardcoded: this script is copied into both architecture
    # images, and the x86_64 path it used to name does not exist on arm64.
    case "$(uname -m)" in
      aarch64|arm64) MULTIARCH=aarch64-linux-gnu ;;
      *)             MULTIARCH=x86_64-linux-gnu ;;
    esac
    mkdir -p $DIR/{bin,lib/$MULTIARCH,lib64,etc,var,usr/lib/$MULTIARCH}
    mkdir -p $DIR/usr/bin
    ## copy useful binary file
    i=0
    while [[ i -lt ${#BIN_ARR[@]} ]]; do
	    cp  /bin/${BIN_ARR[i]} $DIR/bin
        list=`ldd /bin/${BIN_ARR[i]} | egrep -o '[/usr]*/lib.*\.[0-9]+'`
        # mkdir -p first: a library's directory has to exist before it can be
        # copied into, and the fixed list above does not cover every path ldd
        # reports. Without this the first copy into a missing directory fails,
        # the loop carries on, and the chroot ends up with binaries and no
        # libraries — which is what happened on arm64.
        for j in $list; do mkdir -p "$DIR/$(dirname $j)"; cp $j  $DIR/$j; done
        let i++
    done

    i=0
    while [[ i -lt ${#USR_BIN_ARR[@]} ]]; do
	    cp  /usr/bin/${USR_BIN_ARR[i]} $DIR/usr/bin
        list=`ldd /usr/bin/${USR_BIN_ARR[i]} | egrep -o '[/usr]*/lib.*\.[0-9]+'`
        # mkdir -p first: a library's directory has to exist before it can be
        # copied into, and the fixed list above does not cover every path ldd
        # reports. Without this the first copy into a missing directory fails,
        # the loop carries on, and the chroot ends up with binaries and no
        # libraries — which is what happened on arm64.
        for j in $list; do mkdir -p "$DIR/$(dirname $j)"; cp $j  $DIR/$j; done
        let i++
    done

    cp  /bin/kubectl  $DIR/bin/
    cp /etc/hosts $DIR/etc/hosts
    cp /etc/resolv.conf $DIR/etc/resolv.conf
fi

## download the kubeconfig of the cluster this session belongs to
KUBECUBE_URL=${PIVOT_CUBE_URL:-https://kubecube:7443}
KUBECONFIG_URL="${KUBECUBE_URL}/api/v1/cube/user/kubeconfigs?cluster=${CLUSTER}"

wget -d "${headers[@]}" "${KUBECONFIG_URL}" -O $DIR/tmp/$TMP_CONFIG_NAME-base64 --no-check-certificate &> /dev/null
# check whether kubeconfig download success; a deployment that terminates TLS
# elsewhere answers on plain http
if [ $? -ne 0 ]; then
    wget -d "${headers[@]}" "${KUBECONFIG_URL/https:/http:}" -O $DIR/tmp/$TMP_CONFIG_NAME-base64 --no-check-certificate &> /dev/null
    if [ $? -ne 0 ]; then
    exit 1
    fi
fi

CONFIG_BASE64=$(cat $DIR/tmp/$TMP_CONFIG_NAME-base64)
STRING_TMP=${CONFIG_BASE64#"\""}
STRING_TMP=${STRING_TMP%"\""}
echo $STRING_TMP | base64 -d > $DIR/tmp/$TMP_CONFIG_NAME

mv -f $DIR/tmp/$TMP_CONFIG_NAME $DIR/root/.kube/config
rm -rf $DIR/tmp/$TMP_CONFIG_NAME
rm -rf $DIR/tmp/$TMP_CONFIG_NAME-base64

#create group for account if not exists
egrep "^$USERNAME" /etc/group >& /dev/null
if [ $? -ne 0 ]
then
    groupadd $USERNAME
fi

#create user for account if not exists
egrep "^$USERNAME" /etc/passwd >& /dev/null
if [ $? -ne 0 ]
then
    useradd -g $USERNAME $USERNAME
fi

## change owner of director to make account able to write
chown $USERNAME:$USERNAME {$DIR,$DIR/tmp,$DIR/var,$DIR/root}

chroot --userspec=$USERNAME:$USERNAME $DIR /bin/bash

echo "set nocp" >> $DIR/root/.vimrc
source $DIR/root/.vimrc