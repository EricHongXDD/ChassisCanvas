#!/bin/sh

# 微雪 7 英寸屏 CaseScreen USB Gadget 配置脚本。
# 在保留原 CDC ACM 串口（/dev/ttyGS0）的同时增加 RNDIS 网卡。
# 仅操作原固件使用的 ACM gadget；失败时回滚为原始 ACM-only 配置。

set -eu

CONFIGFS="/sys/kernel/config"
GADGET_ROOT="$CONFIGFS/usb_gadget"
GADGET="$GADGET_ROOT/ACM"
CONFIG="$GADGET/configs/c.1"
ACM_FUNCTION="$GADGET/functions/acm.usb0"
RNDIS_FUNCTION="$GADGET/functions/rndis.usb0"
ACM_LINK="$CONFIG/acm.usb0"
RNDIS_LINK="$CONFIG/rndis.usb0"

USB_DEVICE_ADDRESS="192.168.77.2"
USB_HOST_ADDRESS="192.168.77.1"
USB_NETMASK="255.255.255.0"
USB_PREFIX="24"
USB_HOST_MAC="02:77:00:00:00:01"
USB_DEVICE_MAC="02:77:00:00:00:02"
BRIDGE_READY_MARKER="/run/casescreen/usb-gadget-bridge-ready"
UDHCPC="/sbin/udhcpc"
UDHCPC_SCRIPT="/usr/share/udhcpc/default.script"

DNSMASQ="/usr/sbin/dnsmasq"
DNSMASQ_PID="/var/run/casescreen-usb-dnsmasq.pid"
DNSMASQ_LEASES="/tmp/casescreen-usb.leases"

ROLLBACK_REQUIRED=0
ROLLBACK_UDC=""
SKIP_PRIVATE_DHCP=0

log() {
    printf '%s\n' "CaseScreen USB: $*" >&2
}

write_value() {
    value=$1
    path=$2
    printf '%s\n' "$value" > "$path"
}

read_udc() {
    if [ -r "$GADGET/UDC" ]; then
        cat "$GADGET/UDC" 2>/dev/null || true
    fi
}

select_udc() {
    current_udc=$(read_udc)
    if [ -n "$current_udc" ]; then
        printf '%s\n' "$current_udc"
        return 0
    fi

    # 只选择内核公开的 UDC 名称，不解绑或修改其他 gadget。
    for udc_path in /sys/class/udc/*; do
        [ -d "$udc_path" ] || continue
        printf '%s\n' "${udc_path##*/}"
        return 0
    done
    return 1
}

ensure_configfs() {
    if grep -qs " $CONFIGFS configfs " /proc/mounts; then
        return 0
    fi

    mkdir -p "$CONFIGFS" || return 1
    mount -t configfs none "$CONFIGFS" || return 1
    grep -qs " $CONFIGFS configfs " /proc/mounts
}

ensure_link() {
    target=$1
    link=$2

    if [ -L "$link" ]; then
        return 0
    fi
    if [ -e "$link" ]; then
        log "拒绝覆盖非符号链接：$link"
        return 1
    fi
    ln -s "$target" "$link"
}

unbind_target_gadget() {
    current_udc=$(read_udc)
    if [ -n "$current_udc" ]; then
        write_value "" "$GADGET/UDC"
    fi
}

bind_target_gadget() {
    udc_name=$1
    [ -n "$udc_name" ] || return 1
    write_value "$udc_name" "$GADGET/UDC"
}

stop_private_dnsmasq() {
    [ -r "$DNSMASQ_PID" ] || return 0

    dnsmasq_pid=$(cat "$DNSMASQ_PID" 2>/dev/null || true)
    case "$dnsmasq_pid" in
        ''|*[!0-9]*)
            log "忽略无效的专用 dnsmasq PID 文件"
            rm -f "$DNSMASQ_PID"
            return 0
            ;;
    esac

    # PID 文件可能陈旧；只有命令行确实引用本脚本专用 PID 文件时才允许停止。
    if kill -0 "$dnsmasq_pid" 2>/dev/null && \
       [ -r "/proc/$dnsmasq_pid/cmdline" ] && \
       grep -qs "$DNSMASQ_PID" "/proc/$dnsmasq_pid/cmdline"; then
        kill "$dnsmasq_pid" 2>/dev/null || true
    elif kill -0 "$dnsmasq_pid" 2>/dev/null; then
        log "PID $dnsmasq_pid 不属于 CaseScreen 专用 dnsmasq，拒绝停止"
    fi
    rm -f "$DNSMASQ_PID"
}

restore_original_descriptors() {
    # 与原镜像 serial_uart.sh 保持一致，避免改变既有 COM 设备身份。
    write_value "0x1d6b" "$GADGET/idVendor" || return 1
    write_value "0x0104" "$GADGET/idProduct" || return 1
    write_value "0x0100" "$GADGET/bcdDevice" || return 1
    write_value "0x01" "$GADGET/bcdUSB" || return 1
    [ -e "$GADGET/bDeviceClass" ] && write_value "0x00" "$GADGET/bDeviceClass"
    [ -e "$GADGET/bDeviceSubClass" ] && write_value "0x00" "$GADGET/bDeviceSubClass"
    [ -e "$GADGET/bDeviceProtocol" ] && write_value "0x00" "$GADGET/bDeviceProtocol"

    mkdir -p "$GADGET/strings/0x409" || return 1
    write_value "0123456789ABCDEF" "$GADGET/strings/0x409/serialnumber" || return 1
    write_value "SYK" "$GADGET/strings/0x409/manufacturer" || return 1
    write_value "Virtual Serial Port" "$GADGET/strings/0x409/product" || return 1
}

disable_windows_os_desc() {
    if [ -L "$GADGET/os_desc/c.1" ]; then
        rm -f "$GADGET/os_desc/c.1" || true
    fi
    if [ -e "$GADGET/os_desc/use" ]; then
        write_value "0" "$GADGET/os_desc/use" 2>/dev/null || true
    fi
}

remove_rndis_function() {
    if [ -L "$RNDIS_LINK" ]; then
        rm -f "$RNDIS_LINK" || true
    fi
    disable_windows_os_desc
    if [ -d "$RNDIS_FUNCTION" ]; then
        rmdir "$RNDIS_FUNCTION" 2>/dev/null || true
    fi
}

ensure_acm_only() {
    mkdir -p "$GADGET" || return 1
    restore_original_descriptors || return 1
    mkdir -p "$ACM_FUNCTION" "$CONFIG" || return 1
    ensure_link "$ACM_FUNCTION" "$ACM_LINK" || return 1
    remove_rndis_function
}

rollback_to_acm() {
    udc_name=$1
    log "复合 Gadget 配置失败，正在恢复原 ACM 串口"
    stop_private_dnsmasq || true
    unbind_target_gadget 2>/dev/null || true
    ensure_acm_only || log "ACM 配置恢复不完整，请按 README 执行离线恢复"
    if [ -n "$udc_name" ]; then
        bind_target_gadget "$udc_name" 2>/dev/null || log "无法重新绑定 UDC：$udc_name"
    fi
}

on_exit() {
    exit_code=$?
    trap - 0 1 2 15
    if [ "$ROLLBACK_REQUIRED" = "1" ]; then
        rollback_to_acm "$ROLLBACK_UDC"
    fi
    exit "$exit_code"
}

trap 'on_exit' 0
trap 'exit 130' 1 2 15

configure_windows_os_desc() {
    # Microsoft OS 描述符可提高 Windows 自动匹配 RNDIS 驱动的概率。
    [ -d "$GADGET/os_desc" ] || return 0

    write_value "0xcd" "$GADGET/os_desc/b_vendor_code" || return 1
    write_value "MSFT100" "$GADGET/os_desc/qw_sign" || return 1

    rndis_os_desc="$RNDIS_FUNCTION/os_desc/interface.rndis"
    if [ -d "$rndis_os_desc" ]; then
        write_value "RNDIS" "$rndis_os_desc/compatible_id" || return 1
        write_value "5162001" "$rndis_os_desc/sub_compatible_id" || return 1
    fi

    if [ ! -L "$GADGET/os_desc/c.1" ]; then
        [ ! -e "$GADGET/os_desc/c.1" ] || return 1
        ln -s "$CONFIG" "$GADGET/os_desc/c.1" || return 1
    fi
    write_value "1" "$GADGET/os_desc/use" || return 1
}

configure_composite_descriptors() {
    # 0xEF/0x02/0x01 表示带 IAD 的复合设备，同时沿用原 VID/PID 与序列号。
    write_value "0x1d6b" "$GADGET/idVendor" || return 1
    write_value "0x0104" "$GADGET/idProduct" || return 1
    write_value "0x0100" "$GADGET/bcdDevice" || return 1
    write_value "0x0200" "$GADGET/bcdUSB" || return 1
    write_value "0xEF" "$GADGET/bDeviceClass" || return 1
    write_value "0x02" "$GADGET/bDeviceSubClass" || return 1
    write_value "0x01" "$GADGET/bDeviceProtocol" || return 1

    mkdir -p "$GADGET/strings/0x409" || return 1
    write_value "0123456789ABCDEF" "$GADGET/strings/0x409/serialnumber" || return 1
    write_value "SYK" "$GADGET/strings/0x409/manufacturer" || return 1
    write_value "Virtual Serial Port" "$GADGET/strings/0x409/product" || return 1

    mkdir -p "$CONFIG/strings/0x409" || return 1
    write_value "ACM + RNDIS" "$CONFIG/strings/0x409/configuration" || return 1
}

find_rndis_interface() {
    if [ -r "$RNDIS_FUNCTION/ifname" ]; then
        interface_name=$(cat "$RNDIS_FUNCTION/ifname" 2>/dev/null || true)
        if [ -n "$interface_name" ] && [ -d "/sys/class/net/$interface_name" ]; then
            printf '%s\n' "$interface_name"
            return 0
        fi
    fi

    if [ -d /sys/class/net/usb0 ]; then
        printf '%s\n' "usb0"
        return 0
    fi

    for address_path in /sys/class/net/*/address; do
        [ -r "$address_path" ] || continue
        interface_address=$(cat "$address_path" 2>/dev/null || true)
        if [ "$interface_address" = "$USB_DEVICE_MAC" ]; then
            interface_dir=${address_path%/address}
            printf '%s\n' "${interface_dir##*/}"
            return 0
        fi
    done
    return 1
}

wait_for_composite_devices() {
    wait_count=0
    while [ "$wait_count" -lt 10 ]; do
        if [ -e /dev/ttyGS0 ]; then
            usb_interface=$(find_rndis_interface || true)
            if [ -n "$usb_interface" ]; then
                printf '%s\n' "$usb_interface"
                return 0
            fi
        fi
        wait_count=$((wait_count + 1))
        sleep 1
    done
    return 1
}

configure_usb_address() {
    usb_interface=$1
    /sbin/ifconfig "$usb_interface" "$USB_DEVICE_ADDRESS" netmask "$USB_NETMASK" up || return 1
    /sbin/ip -4 addr show dev "$usb_interface" 2>/dev/null | grep -q "$USB_DEVICE_ADDRESS/$USB_PREFIX"
}

prepare_usb_interface() {
    usb_interface=$1
    if [ "$SKIP_PRIVATE_DHCP" = "1" ]; then
        /sbin/ifconfig "$usb_interface" up
        return
    fi
    configure_usb_address "$usb_interface"
}

bridge_client_ready() {
    usb_interface=$1
    [ -r "$BRIDGE_READY_MARKER" ] || return 1
    grep -qx "interface=$usb_interface gateway=$USB_HOST_ADDRESS" "$BRIDGE_READY_MARKER" || return 1
    /sbin/ip -4 addr show dev "$usb_interface" 2>/dev/null | grep -q 'inet ' || return 1
    grep -Eq "^$usb_interface[[:space:]]+00000000[[:space:]]+014DA8C0[[:space:]]+" /proc/net/route || return 1
    grep -Eq '^[[:space:]]*nameserver[[:space:]]+[^[:space:]#]+' /etc/resolv.conf
}

configure_bridge_client() {
    usb_interface=$1
    stop_private_dnsmasq || true

    # 原厂 qt_app 会连续调用两次 /data/serial_uart.sh。第一次已经取得租约时直接复用，
    # 避免第二次再次发送 DHCP 广播并延长启动时间。
    if bridge_client_ready "$usb_interface"; then
        log "专用 USB 网络桥客户端已就绪；复用现有 DHCP 租约"
        return 0
    fi
    rm -f "$BRIDGE_READY_MARKER"

    [ -x "$UDHCPC" ] || return 1
    [ -x "$UDHCPC_SCRIPT" ] || return 1
    /sbin/ifconfig "$usb_interface" up || return 1
    # 外部专用桥负责 DHCP、DNS 与 NAT。30 秒有界等待失败后由识歌门禁回到原厂，
    # 不会让屏幕在没有桥时永久卡在启动脚本。
    "$UDHCPC" -i "$usb_interface" -s "$UDHCPC_SCRIPT" -n -q -t 15 -T 2 || return 1
    /sbin/ip -4 addr show dev "$usb_interface" 2>/dev/null | grep -q 'inet ' || return 1
    grep -Eq "^$usb_interface[[:space:]]+00000000[[:space:]]+014DA8C0[[:space:]]+" /proc/net/route || return 1
    grep -Eq '^[[:space:]]*nameserver[[:space:]]+[^[:space:]#]+' /etc/resolv.conf || return 1

    marker_directory=${BRIDGE_READY_MARKER%/*}
    marker_temp="$BRIDGE_READY_MARKER.tmp.$$"
    umask 077
    mkdir -p "$marker_directory" || return 1
    printf '%s\n' "interface=$usb_interface gateway=$USB_HOST_ADDRESS" > "$marker_temp" || {
        rm -f "$marker_temp"
        return 1
    }
    chmod 0600 "$marker_temp" || {
        rm -f "$marker_temp"
        return 1
    }
    mv -f "$marker_temp" "$BRIDGE_READY_MARKER" || {
        rm -f "$marker_temp"
        return 1
    }
    log "专用 USB 网络桥客户端已配置；桥端网关 $USB_HOST_ADDRESS，屏端地址由 DHCP 获取"
}

private_dnsmasq_running() {
    [ -r "$DNSMASQ_PID" ] || return 1
    dnsmasq_pid=$(cat "$DNSMASQ_PID" 2>/dev/null || true)
    case "$dnsmasq_pid" in
        ''|*[!0-9]*) return 1 ;;
    esac
    kill -0 "$dnsmasq_pid" 2>/dev/null || return 1
    [ -r "/proc/$dnsmasq_pid/cmdline" ] || return 1
    grep -qs "$DNSMASQ_PID" "/proc/$dnsmasq_pid/cmdline"
}

start_private_dnsmasq() {
    usb_interface=$1

    if private_dnsmasq_running; then
        return 0
    fi
    rm -f "$DNSMASQ_PID"

    [ -x "$DNSMASQ" ] || return 1
    "$DNSMASQ" \
        --conf-file=/dev/null \
        --port=0 \
        --interface="$usb_interface" \
        --bind-interfaces \
        --dhcp-authoritative \
        --dhcp-range="$USB_HOST_ADDRESS,$USB_HOST_ADDRESS,$USB_NETMASK,12h" \
        --dhcp-host="$USB_HOST_MAC,$USB_HOST_ADDRESS,infinite" \
        --dhcp-option=3 \
        --dhcp-option=6 \
        --dhcp-lease-max=1 \
        --dhcp-sequential-ip \
        --pid-file="$DNSMASQ_PID" \
        --dhcp-leasefile="$DNSMASQ_LEASES" || return 1

    private_dnsmasq_running
}

is_composite_bound() {
    [ -L "$ACM_LINK" ] || return 1
    [ -L "$RNDIS_LINK" ] || return 1
    [ -n "$(read_udc)" ]
}

start_composite() {
    ensure_configfs || {
        log "configfs 挂载失败"
        return 1
    }

    if is_composite_bound; then
        existing_interface=$(wait_for_composite_devices || true)
        if [ -n "$existing_interface" ] && prepare_usb_interface "$existing_interface"; then
            if [ "$SKIP_PRIVATE_DHCP" = "1" ]; then
                stop_private_dnsmasq || true
                log "ACM + RNDIS 已就绪；等待专用网络桥配置"
            elif start_private_dnsmasq "$existing_interface"; then
                log "ACM + RNDIS 已就绪；屏端 $USB_DEVICE_ADDRESS，Windows 端预计 $USB_HOST_ADDRESS"
            else
                log "DHCP 未启动；请在 Windows RNDIS 网卡手动设置 $USB_HOST_ADDRESS/$USB_PREFIX（网关和 DNS 留空）"
            fi
            return 0
        fi
        log "已有复合 Gadget 状态异常，尝试安全重建"
    fi

    mkdir -p "$GADGET" || return 1
    ROLLBACK_UDC=$(select_udc) || {
        log "未找到可用 UDC"
        return 1
    }

    ROLLBACK_REQUIRED=1
    stop_private_dnsmasq || true
    unbind_target_gadget || return 1

    mkdir -p "$ACM_FUNCTION" "$RNDIS_FUNCTION" "$CONFIG" || return 1
    configure_composite_descriptors || return 1
    write_value "$USB_HOST_MAC" "$RNDIS_FUNCTION/host_addr" || return 1
    write_value "$USB_DEVICE_MAC" "$RNDIS_FUNCTION/dev_addr" || return 1

    # 链接顺序固定为 ACM 在前，确保原串口仍作为首个功能出现。
    ensure_link "$ACM_FUNCTION" "$ACM_LINK" || return 1
    ensure_link "$RNDIS_FUNCTION" "$RNDIS_LINK" || return 1

    if ! configure_windows_os_desc; then
        log "Windows OS 描述符不可用，将继续使用标准 RNDIS 描述符"
        disable_windows_os_desc
    fi

    bind_target_gadget "$ROLLBACK_UDC" || return 1
    usb_interface=$(wait_for_composite_devices) || {
        log "绑定后未发现 ttyGS0 或 RNDIS 网卡"
        return 1
    }
    prepare_usb_interface "$usb_interface" || {
        log "无法启用 $usb_interface"
        return 1
    }

    # 至此 ACM、RNDIS 与屏端固定地址均有效，后续 DHCP 失败不需要回滚。
    ROLLBACK_REQUIRED=0

    if [ "$SKIP_PRIVATE_DHCP" = "1" ]; then
        stop_private_dnsmasq || true
        log "ACM + RNDIS 已就绪；等待专用网络桥配置"
    elif start_private_dnsmasq "$usb_interface"; then
        log "ACM + RNDIS 已就绪；屏端 $USB_DEVICE_ADDRESS，Windows 端预计 $USB_HOST_ADDRESS"
    else
        log "DHCP 未启动；请在 Windows RNDIS 网卡手动设置 $USB_HOST_ADDRESS/$USB_PREFIX（网关和 DNS 留空）"
    fi
}

restore_acm_only() {
    ensure_configfs || return 1
    mkdir -p "$GADGET" || return 1
    ROLLBACK_UDC=$(select_udc) || {
        log "未找到可用 UDC"
        return 1
    }

    ROLLBACK_REQUIRED=1
    rm -f "$BRIDGE_READY_MARKER"
    stop_private_dnsmasq || true
    unbind_target_gadget || return 1
    ensure_acm_only || return 1
    bind_target_gadget "$ROLLBACK_UDC" || return 1
    ROLLBACK_REQUIRED=0
    log "已恢复原始 ACM-only 模式；RNDIS 已移除"
}

show_status() {
    if [ ! -d "$GADGET" ]; then
        printf '%s\n' "gadget=absent"
        return 1
    fi

    printf '%s\n' "gadget=ACM"
    printf '%s\n' "udc=$(read_udc)"
    [ -L "$ACM_LINK" ] && printf '%s\n' "acm=enabled" || printf '%s\n' "acm=disabled"
    [ -L "$RNDIS_LINK" ] && printf '%s\n' "rndis=enabled" || printf '%s\n' "rndis=disabled"

    status_interface=$(find_rndis_interface || true)
    if [ -n "$status_interface" ]; then
        printf '%s\n' "interface=$status_interface"
        /sbin/ip -4 addr show dev "$status_interface" 2>/dev/null || true
    else
        printf '%s\n' "interface=absent"
    fi

    if private_dnsmasq_running; then
        printf '%s\n' "dhcp=running"
    else
        printf '%s\n' "dhcp=stopped"
        printf '%s\n' "windows_static=$USB_HOST_ADDRESS/$USB_PREFIX"
    fi
}

case "${1:-start}" in
    start)
        start_composite
        ;;
    restore-acm)
        restore_acm_only
        ;;
    bridge-client)
        SKIP_PRIVATE_DHCP=1
        start_composite
        bridge_interface=$(find_rndis_interface || true)
        [ -n "$bridge_interface" ] || {
            log "找不到 RNDIS 网卡，无法启用专用网络桥客户端"
            if ! restore_acm_only; then
                log "专用桥失败后恢复 ACM-only 也失败"
            fi
            exit 1
        }
        configure_bridge_client "$bridge_interface" || {
            log "专用网络桥路由或 DNS 配置失败"
            if ! restore_acm_only; then
                log "专用桥失败后恢复 ACM-only 也失败"
            fi
            exit 1
        }
        ;;
    status)
        # status 不挂载、不写入，仅报告当前状态。
        show_status
        ;;
    *)
        printf '用法：%s {start|bridge-client|restore-acm|status}\n' "$0" >&2
        exit 2
        ;;
esac
