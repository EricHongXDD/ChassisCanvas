#!/bin/sh

# 在采用 NetworkManager 的 Raspberry Pi OS / Debian 小主机上建立专用 RNDIS 上网桥。
# Wi-Fi 凭据仍由系统自己的连接界面管理，本脚本不会读取或保存无线密码。

set -eu

PROFILE_NAME="CaseScreen USB Bridge"
PROFILE_MARKER="casescreen-usb-bridge-v1"
HOST_MAC="02:77:00:00:00:01"
BRIDGE_ADDRESS="192.168.77.1/24"

log() {
    printf '%s\n' "CaseScreen bridge: $*" >&2
}

require_root() {
    [ "$(id -u)" = "0" ] || {
        log "请使用 sudo 运行"
        exit 1
    }
}

require_network_manager() {
    command -v nmcli >/dev/null 2>&1 || {
        log "缺少 nmcli；请使用启用了 NetworkManager 的 Raspberry Pi OS Bookworm 或 Debian"
        exit 1
    }
    nmcli -t -f RUNNING general 2>/dev/null | grep -qx running || {
        log "NetworkManager 未运行"
        exit 1
    }
}

profile_exists() {
    nmcli -t -f NAME connection show 2>/dev/null | grep -Fxq "$PROFILE_NAME"
}

profile_owned() {
    [ "$(nmcli -g connection.type connection show "$PROFILE_NAME" 2>/dev/null || true)" = "802-3-ethernet" ] || return 1
    [ "$(nmcli -g connection.stable-id connection show "$PROFILE_NAME" 2>/dev/null || true)" = "$PROFILE_MARKER" ]
}

install_profile() {
    require_root
    require_network_manager

    if profile_exists; then
        profile_owned || {
            log "同名连接不属于 CaseScreen，拒绝覆盖：$PROFILE_NAME"
            exit 1
        }
    else
        nmcli connection add \
            type ethernet \
            ifname "*" \
            con-name "$PROFILE_NAME" \
            connection.stable-id "$PROFILE_MARKER" \
            802-3-ethernet.mac-address "$HOST_MAC" >/dev/null
    fi

    # ipv4.method=shared 由 NetworkManager 提供 DHCP、DNS 转发和到现有 Wi-Fi 上行的 NAT。
    # never-default 防止 USB 私网被误选为小桥自身的互联网出口。
    nmcli connection modify "$PROFILE_NAME" \
        connection.autoconnect yes \
        connection.autoconnect-priority 50 \
        connection.stable-id "$PROFILE_MARKER" \
        802-3-ethernet.mac-address "$HOST_MAC" \
        ipv4.method shared \
        ipv4.addresses "$BRIDGE_ADDRESS" \
        ipv4.never-default yes \
        ipv6.method disabled

    log "配置已安装；把小桥连上 Wi-Fi，再将微雪屏的 USB 数据口接到小桥 USB Host 口"
    log "屏幕枚举后 NetworkManager 会自动提供 192.168.77.1 网关、DHCP、DNS 与 NAT"
}

remove_profile() {
    require_root
    require_network_manager
    if profile_exists; then
        profile_owned || {
            log "同名连接不属于 CaseScreen，拒绝删除：$PROFILE_NAME"
            exit 1
        }
        nmcli connection delete "$PROFILE_NAME" >/dev/null
        log "已删除专用 USB 网络桥配置"
    else
        log "专用 USB 网络桥配置不存在"
    fi
}

show_status() {
    require_network_manager
    if ! profile_exists; then
        printf '%s\n' "profile=absent"
        exit 1
    fi
    printf '%s\n' "profile=present"
    if profile_owned; then
        printf '%s\n' "owned=yes"
    else
        printf '%s\n' "owned=no"
        log "同名连接不属于 CaseScreen，不会修改或删除"
        exit 1
    fi
    nmcli -f NAME,TYPE,DEVICE,STATE connection show "$PROFILE_NAME"
    printf '%s\n' "expected_host_mac=$HOST_MAC"
    printf '%s\n' "gateway=$BRIDGE_ADDRESS"
}

case "${1:-install}" in
    install) install_profile ;;
    remove) remove_profile ;;
    status) show_status ;;
    *)
        printf '用法：%s {install|remove|status}\n' "$0" >&2
        exit 2
        ;;
esac
