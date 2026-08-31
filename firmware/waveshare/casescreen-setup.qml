import QtQuick 2.12
import QtQuick.Window 2.12

Window {
    id: root
    visible: true
    visibility: Window.FullScreen
    color: "#071318"
    title: "CaseScreen 一次性设置"

    property var arguments: Qt.application.arguments
    property string setupUrl: arguments.length >= 3 ? arguments[arguments.length - 3] : "入口不可用"
    property string setupCode: arguments.length >= 2 ? arguments[arguments.length - 2] : "--------"
    property string expiresAt: arguments.length >= 1 ? arguments[arguments.length - 1] : "--:--:--"
    property real shortSide: Math.min(width, height)
    property real pageMargin: Math.max(24, shortSide * 0.055)

    Rectangle {
        anchors.fill: parent
        gradient: Gradient {
            GradientStop { position: 0.0; color: "#071318" }
            GradientStop { position: 1.0; color: "#12313a" }
        }
    }

    Column {
        anchors.fill: parent
        anchors.margins: root.pageMargin
        spacing: Math.max(14, root.shortSide * 0.026)

        Text {
            width: parent.width
            text: "CaseScreen 一次性设置"
            color: "#e9ffff"
            font.pixelSize: Math.max(30, root.shortSide * 0.075)
            font.bold: true
            wrapMode: Text.Wrap
        }

        Text {
            width: parent.width
            text: "用手机或电脑连接屏幕的受保护网络，然后在浏览器打开："
            color: "#9fc3c9"
            font.pixelSize: Math.max(18, root.shortSide * 0.041)
            wrapMode: Text.Wrap
        }

        Rectangle {
            width: parent.width
            height: urlText.implicitHeight + root.pageMargin
            radius: Math.max(10, root.shortSide * 0.02)
            color: "#0b2128"
            border.color: "#2e5962"
            border.width: 2

            Text {
                id: urlText
                anchors.centerIn: parent
                width: parent.width - root.pageMargin
                text: root.setupUrl
                color: "#80ebe2"
                font.pixelSize: Math.max(20, root.shortSide * 0.05)
                font.bold: true
                horizontalAlignment: Text.AlignHCenter
                wrapMode: Text.WrapAnywhere
            }
        }

        Text {
            width: parent.width
            text: "一次性设置码"
            color: "#9fc3c9"
            font.pixelSize: Math.max(18, root.shortSide * 0.041)
            horizontalAlignment: Text.AlignHCenter
        }

        Text {
            width: parent.width
            text: root.setupCode
            color: "#ffffff"
            font.pixelSize: Math.max(48, root.shortSide * 0.13)
            font.bold: true
            font.letterSpacing: Math.max(3, root.shortSide * 0.009)
            horizontalAlignment: Text.AlignHCenter
        }

        Text {
            width: parent.width
            text: "窗口截止 " + root.expiresAt + "。设置页只接收本地 Wi-Fi 密码；不会接收 QQ/网易账号、密码或 Cookie。"
            color: "#ffcf8a"
            font.pixelSize: Math.max(16, root.shortSide * 0.035)
            horizontalAlignment: Text.AlignHCenter
            wrapMode: Text.Wrap
        }

        Text {
            width: parent.width
            text: "若屏幕没有无线硬件，请通过 USB-RNDIS 的 192.168.77.2 入口完成一次性设置。"
            color: "#8eabb1"
            font.pixelSize: Math.max(14, root.shortSide * 0.03)
            horizontalAlignment: Text.AlignHCenter
            wrapMode: Text.Wrap
        }
    }
}
