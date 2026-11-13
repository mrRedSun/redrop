import type { BaseSyntheticEvent } from 'react'
import { Exit, Schema } from 'effect'
import { Atom } from 'effect/reactivity'
import { useAtomSet } from '@effect/atom-react'
import { standardSchemaResolver } from '@hookform/resolvers/standard-schema'
import type { FieldValues, Resolver, UseFormReturn } from 'react-hook-form'
import { describeCause } from '@/api/errors'

/**
 * react-hook-form resolver from an Effect Schema. The form holds the
 * schema's Encoded side (strings from inputs); handleSubmit receives Type.
 */
export function schemaResolver<T, I extends FieldValues>(schema: Schema.Codec<T, I>): Resolver<I, unknown, T> {
  return standardSchemaResolver(Schema.toStandardSchemaV1(schema))
}

interface SubmitOptions<T, Arg, A> {
  /** Form output -> mutation argument, when they differ (e.g. adding the step-1 credentials). */
  readonly toArg: (values: T) => Arg
  readonly onSuccess?: (value: A, values: T) => void
}

/**
 * Binds a form to a mutation atom. Failures land on the form (`root.server`,
 * rendered by <FormRootError/>), so the atom should use `failures: 'form'`;
 * success calls `onSuccess` with the server's answer.
 */
export function useFormSubmit<I extends FieldValues, T, A, E>(
  form: UseFormReturn<I, unknown, T>,
  mutation: Atom.AtomResultFn<T, A, E>,
  options?: Omit<SubmitOptions<T, T, A>, 'toArg'>,
): (event?: BaseSyntheticEvent) => Promise<void>
export function useFormSubmit<I extends FieldValues, T, Arg, A, E>(
  form: UseFormReturn<I, unknown, T>,
  mutation: Atom.AtomResultFn<Arg, A, E>,
  options: SubmitOptions<T, Arg, A>,
): (event?: BaseSyntheticEvent) => Promise<void>
export function useFormSubmit<I extends FieldValues, T, A, E>(
  form: UseFormReturn<I, unknown, T>,
  mutation: Atom.AtomResultFn<unknown, A, E>,
  options?: Partial<SubmitOptions<T, unknown, A>>,
): (event?: BaseSyntheticEvent) => Promise<void> {
  const run = useAtomSet(mutation, { mode: 'promiseExit' })
  const reset = useAtomSet(mutation)
  return form.handleSubmit(async (values) => {
    try {
      const exit = await run(options?.toArg ? options.toArg(values) : values)
      if (Exit.isSuccess(exit)) options?.onSuccess?.(exit.value, values)
      else form.setError('root.server', { message: describeCause(exit.cause) })
    } finally {
      // Atom.fn retains the last argument as well as its result. Forms may
      // submit passwords; one-time results have already been copied by the
      // caller. Clear both immediately, on success and on failure.
      reset(Atom.Reset)
    }
  })
}
