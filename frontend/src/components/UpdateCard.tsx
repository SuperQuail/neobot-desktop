import {useEffect, useState} from 'react'
import {Button, Card, Typography} from 'antd'
import {CheckUpdate, DownloadUpdate, LaunchUpdate, Quit} from '../../wailsjs/go/main/App'
import {BrowserOpenURL} from '../../wailsjs/runtime/runtime'
import {useLauncher} from '../app/LauncherProvider'
import {APP_VERSION} from '../app/version'

// 状态行只说「现在是什么情况」，时间戳按远近缩短：今天只显示时分，往年才带日期。
function formatCheckedAt(seconds: number): string {
    if (!seconds) return ''
    const at = new Date(seconds * 1000)
    if (Number.isNaN(at.getTime())) return ''
    const pad = (value: number) => String(value).padStart(2, '0')
    const time = `${pad(at.getHours())}:${pad(at.getMinutes())}`
    const now = new Date()
    const sameDay = at.getFullYear() === now.getFullYear() &&
        at.getMonth() === now.getMonth() && at.getDate() === now.getDate()
    return sameDay ? time : `${at.getMonth() + 1}/${at.getDate()} ${time}`
}

// Wails 会把 Go 的 error 原文带到前端。这里只放行可读的中文提示，
// 其余的（例如浏览器预览时的 TypeError）换成一句通用说明，不把原始异常抛给用户。
function readable(failure: unknown): string {
    const text = typeof failure === 'string' ? failure : failure instanceof Error ? failure.message : ''
    return /[\u4e00-\u9fff]/.test(text) ? text : '更新没有完成，请检查网络后重试'
}

export function UpdateCard() {
    const {state, refresh} = useLauncher()
    const [busy, setBusy] = useState<'' | 'download' | 'install'>('')
    const [note, setNote] = useState('')

    useEffect(() => {
        // 后端的 1 小时节流会挡住重复的自动检查，这里不用自己记时间。
        void CheckUpdate(false).catch(() => undefined)
    }, [])

    if (!state) return null
    const update = state.update
    const downloaded = Boolean(update.downloaded)
    const downloadable = Boolean(update.auto_update && update.asset_url && update.checksums_url)
    const checkedAt = formatCheckedAt(update.checked_at)
    const idle = busy === ''

    // 一句话交代现状。失败时不能声称「已是最新版本」：那次检查并没有得出结论。
    const tone = note || update.error ? 'tone-warn'
        : downloaded ? 'tone-ok' : update.available ? 'tone-warn' : 'tone-idle'
    const status = note ? note
        : update.checking ? '正在检查更新…'
            : downloaded ? '安装包已下载并通过校验'
                : update.available ? `发现新版本 v${update.latest}`
                    : update.error ? update.error
                        : (checkedAt ? `已是最新版本 · 上次检查 ${checkedAt}` : '尚未检查更新')
    const hint = busy === 'download' ? '正在下载并校验安装包。'
        : update.available && !update.auto_update ? '当前系统请到发布页下载更新包。'
            : update.available && !downloaded && !downloadable ? '此版本缺少安装包或校验文件，请查看最新发布页。'
            : update.available && !downloaded ? (update.notes || '下载完成后，点击「退出并安装」开始更新。')
                : update.error && checkedAt ? `上次检查 ${checkedAt}；网络或接口恢复后可以再试。`
                    : ''

    async function check() {
        setNote('')
        try {
            await CheckUpdate(true)
            await refresh()
        } catch (failure) {
            setNote(readable(failure))
        }
    }

    async function download() {
        if (!idle) return
        setBusy('download')
        setNote('')
        try {
            await DownloadUpdate()
            await refresh()
        } catch (failure) {
            setNote(readable(failure))
        } finally {
            setBusy('')
        }
    }

    async function install() {
        if (!idle) return
        setBusy('install')
        setNote('')
        try {
            await LaunchUpdate()
        } catch (failure) {
            setNote(readable(failure))
            setBusy('')
            return
        }
        // 安装程序要覆盖正在运行的程序文件，本进程必须尽快退出，
        // 否则安装包在复制文件时会因为文件被占用而失败。
        try {
            await Quit()
        } catch {
            setNote('安装包已启动，请手动退出启动器完成安装。')
            setBusy('')
        }
    }

    return <Card className="stack-card" size="small" title="关于" extra={
        <span className="info-row-value tabular">v{APP_VERSION}</span>
    }>
        <div className="info-row">
            <div className="info-row-main">
                <span className={'state-dot ' + tone} role="status"><i aria-hidden="true" />{status}</span>
                {hint && <div className="info-row-hint">{hint}</div>}
            </div>
            <div className="info-row-side">
                {downloaded
                    ? <Button size="small" type="primary" loading={busy === 'install'}
                        disabled={!idle} onClick={() => void install()}>退出并安装</Button>
                    : update.available && !update.auto_update
                        ? <>
                            <Button size="small" type="text" loading={update.checking} disabled={!idle || busy !== ''}
                                onClick={() => void check()}>重新检查</Button>
                            <Button size="small" type="primary" disabled={!update.page_url}
                                onClick={() => update.page_url && BrowserOpenURL(update.page_url)}>打开发布页</Button>
                        </>
                        : update.available && downloadable
                            ? <>
                                <Button size="small" type="text" loading={update.checking} disabled={!idle || busy !== ''}
                                    onClick={() => void check()}>重新检查</Button>
                                <Button size="small" type="primary" loading={busy === 'download'}
                                    disabled={!idle || update.checking} onClick={() => void download()}>下载更新</Button>
                            </>
                            : <Button size="small" type="text" loading={update.checking}
                                disabled={!idle} onClick={() => void check()}>检查更新</Button>}
            </div>
        </div>

    </Card>
}
