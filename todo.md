
# macos上执行codex exec命令，需要使用sandbox-exec来执行

CODEX_HOME的格式=/xxx/{用户}/.codex，从而可以获取"/xxx/{用户}"和"/xxx"这两个路径。
然后使用以下方式来运行
```bash
sandbox-exec -p '
(version 1)
(allow default)
(deny file-read* file-write*
    (require-all
        (subpath "/xxx")
        (require-not
            (subpath "/xxx/{用户}"))
    )
)
(allow file-read-metadata
    (literal "/xxx"))
' codex exec --skip-git-repo-check --sandbox danger-full-access
```
