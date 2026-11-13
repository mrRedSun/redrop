import type { ReactNode } from 'react'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { cn } from '@/lib/utils'

export type Column<T> = {
  header: ReactNode
  cell: (row: T) => ReactNode
  className?: string
}

type Props<T> = {
  columns: Column<T>[]
  rows: T[] | undefined
  rowKey: (row: T) => string
  /** Shown when rows is an empty array. */
  empty: ReactNode
  /** Dims rows while newer data loads (e.g. a filter change). */
  stale?: boolean
}

/** Generic table: skeleton while rows are undefined, empty state when none. */
export function DataTable<T>({ columns, rows, rowKey, empty, stale }: Props<T>) {
  if (rows && rows.length === 0) return <>{empty}</>
  return (
    <div className={cn('rounded-lg border transition-opacity', stale && 'opacity-60')}>
      <Table>
        <TableHeader>
          <TableRow>
            {columns.map((c, i) => (
              <TableHead key={i} className={c.className}>
                {c.header}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows
            ? rows.map((row) => (
                <TableRow key={rowKey(row)}>
                  {columns.map((c, i) => (
                    <TableCell key={i} className={c.className}>
                      {c.cell(row)}
                    </TableCell>
                  ))}
                </TableRow>
              ))
            : Array.from({ length: 3 }, (_, r) => (
                <TableRow key={r}>
                  {columns.map((c, i) => (
                    <TableCell key={i} className={c.className}>
                      <Skeleton className="h-4 w-full max-w-32" />
                    </TableCell>
                  ))}
                </TableRow>
              ))}
        </TableBody>
      </Table>
    </div>
  )
}
