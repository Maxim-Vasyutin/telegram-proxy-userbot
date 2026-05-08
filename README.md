# Telegram Proxy Userbot

Go-юзербот, который прозрачно ретранслирует сообщения, ответы, реакции, а также события редактирования и удаления между парами Telegram-групп («мерчант» и «саппорт»). Агенты поддержки работают в своих группах и никогда не появляются в группах мерчанта — всё общение мостируется юзерботом от его имени.

## Быстрый старт

```bash
# 1. Скопируй и отредактируй конфигурационные файлы
cp config.example.yaml config.yaml
cp .env.example .env
# Заполни config.yaml (реальные chat_id, номера телефонов, пути к сессиям)
# Заполни .env (TELEGRAM_API_ID, TELEGRAM_API_HASH, DATABASE_DSN)

# 2. Запусти весь стек
docker compose -f deploy/docker-compose.yml up -d
```

Просмотр логов:

```bash
docker compose -f deploy/docker-compose.yml logs -f userbot
```

Проверка конфига без запуска:

```bash
docker compose -f deploy/docker-compose.yml run --rm userbot ./userbot --check
```

## Настройка базы данных

Применить миграции через [goose](https://github.com/pressly/goose):

```bash
goose -dir migrations postgres "$DATABASE_DSN" up
```

Откатить последнюю миграцию:

```bash
goose -dir migrations postgres "$DATABASE_DSN" down
```

## Первичная авторизация

Каждый аккаунт Telegram должен быть авторизован **один раз** перед тем, как сервис сможет работать в автономном режиме. Флаг `--auth` запускает интерактивный процесс: запрашивает код входа (и облачный пароль 2FA, если он задан), сохраняет сессию на диск и завершается.

Что нужно перед запуском:

1. Реальные Telegram API-учётные данные в `.env` — получи на
   <https://my.telegram.org/apps>, заполни `TELEGRAM_API_ID` и `TELEGRAM_API_HASH`.
2. Номер телефона должен быть прописан в `config.yaml` в секции `accounts:`
   с доступным для записи путём `session_path`.
3. Postgres должен быть запущен и миграции применены (см. выше) —
   `--auth` сам не обращается к базе, но это нужно перед `up -d`.

Запусти авторизацию внутри контейнера с флагами `--rm` (удалить контейнер после) и `-it` (подключить stdin для интерактивных запросов):

```bash
docker compose -f deploy/docker-compose.yml run --rm -it userbot \
  ./userbot --auth --phone +79001234567
```

Что происходит:

1. Юзербот подключается к Telegram и запрашивает отправку кода входа
   на твой телефон (код придёт в официальном приложении Telegram,
   **не** через SMS, если на аккаунте уже есть активная сессия).
2. CLI выводит приглашение:

   ```text
   Enter the login code Telegram sent to +79001234567:
   ```

   Введи 5-значный код и нажми Enter.

3. Если на аккаунте включён облачный пароль (2FA), появится второй запрос
   с маскированным вводом:

   ```text
   Enter the cloud (2FA) password for +79001234567:
   ```

4. При успехе бинарник выводит в лог `auth completed` (JSON) и завершается
   с кодом `0`. Файл сессии записывается по пути из `config.yaml`
   (обычно `/var/lib/userbot/sessions/account_<phone>.session` внутри
   Docker-тома `userbot_sessions`).

5. Если сессия уже валидна (например, при повторном запуске `--auth`),
   бинарник выводит `session already valid` и завершается с кодом `0`
   без запросов.

Повтори команду для каждой записи в секции `accounts:` файла `config.yaml`,
затем запусти сервис в штатном режиме:

```bash
docker compose -f deploy/docker-compose.yml up -d
```

### Решение проблем с авторизацией

- **«phone not found in config.accounts»** — значение в `--phone` должно
  совпадать с `accounts[].phone` в `config.yaml` байт в байт (включая ведущий `+`).
- **Сессия отозвана** («Завершить все другие сессии» нажато на телефоне) —
  удали соответствующий файл `*.session` из тома `userbot_sessions`
  и повтори `--auth`.
- **Нет возможности запустить интерактивно (CI / не-TTY)** — `--auth`
  требует TTY для маскировки пароля. Запусти вручную на рабочей станции
  или VPS, затем скопируй файлы `*.session` на продакшн-машину.
